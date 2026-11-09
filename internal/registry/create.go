package registry

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

//go:embed templates/*.tmpl
var templates embed.FS

type CreatorOptions struct {
	Dir     string // the tickets directory
	States  *state.Manager
	Sources *ticket.Registry
	// RepoCheck says whether a service's repository is usable; nil requires an existing directory.
	RepoCheck    func(path string) error
	BranchPrefix string
}

// Creator makes new ticket directories.
type Creator struct {
	opts CreatorOptions
	reg  *Registry
}

func NewCreator(o CreatorOptions) *Creator {
	return &Creator{opts: o, reg: New(o.Dir, o.States)}
}

const yamlHeader = `# Edit freely. Gates, push mode, sandbox, ports and more can be set here; see docs/formats.md.
`

// Create validates everything first, then claims the ticket directory atomically (so
// concurrent creates of one id have exactly one winner and an existing ticket is never
// touched), then writes the files. A failure after the claim removes the directory.
func (c *Creator) Create(ctx context.Context, p api.TicketNewParams) (api.TicketNewResult, error) {
	seed, source, err := c.seed(ctx, p)
	if err != nil {
		return api.TicketNewResult{}, err
	}
	t := domain.Ticket{ID: seed.ID, Title: seed.Title, Source: source}
	for _, s := range p.Services {
		branch := s.Branch
		if branch == "" {
			branch = c.opts.BranchPrefix + seed.ID + "/" + s.Name
		}
		t.Services = append(t.Services, domain.Service{Name: s.Name, Repo: s.Repo, Branch: branch})
	}
	doc, err := yaml.Marshal(t)
	if err != nil {
		return api.TicketNewResult{}, fmt.Errorf("encode ticket.yaml: %w", err)
	}
	doc = append([]byte(yamlHeader), doc...)
	checked, err := config.ParseTicket(doc, config.TicketOptions{RepoCheck: c.opts.RepoCheck})
	if err != nil {
		return api.TicketNewResult{}, &api.Error{Code: api.CodeInvalid, Message: err.Error()}
	}
	t = checked // with defaults applied, such as each service's path

	dir := filepath.Join(c.opts.Dir, t.ID)
	if err := os.MkdirAll(c.opts.Dir, 0o700); err != nil {
		return api.TicketNewResult{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return api.TicketNewResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q already exists", t.ID)}
		}
		return api.TicketNewResult{}, err
	}
	if err := c.write(dir, t, seed, doc); err != nil {
		os.RemoveAll(dir)
		return api.TicketNewResult{}, err
	}

	res, err := c.reg.Get(t.ID)
	if err != nil {
		return api.TicketNewResult{}, err
	}
	return api.TicketNewResult{Summary: res.Summary, Path: dir}, nil
}

// seed works out the ticket's id, title and description from the source named in
// p.From (if any), with anything the caller set explicitly taking precedence.
func (c *Creator) seed(ctx context.Context, p api.TicketNewParams) (ticket.Seed, domain.Source, error) {
	source := domain.Source{Type: "manual"}
	var seed ticket.Seed
	if p.From == "" {
		var err error
		seed, err = ticket.Manual{ID: p.ID, Title: p.Title, Description: p.Description}.Fetch(ctx, p.Title)
		if err != nil {
			return ticket.Seed{}, source, &api.Error{Code: api.CodeInvalid, Message: err.Error()}
		}
		return seed, source, nil
	}

	typ, ref, ok := strings.Cut(p.From, ":")
	if !ok || typ == "" || ref == "" {
		return ticket.Seed{}, source, &api.Error{Code: api.CodeInvalid, Message: fmt.Sprintf("from %q must look like type:ref, for example jira:PROJ-412", p.From)}
	}
	seed, err := c.opts.Sources.Fetch(ctx, typ, ref)
	switch {
	case errors.Is(err, ticket.ErrNotFound):
		return ticket.Seed{}, source, &api.Error{Code: api.CodeNotFound, Message: err.Error()}
	case errors.Is(err, ticket.ErrUnknownSource), errors.Is(err, ticket.ErrUnusable), errors.Is(err, ticket.ErrNotConfigured):
		return ticket.Seed{}, source, &api.Error{Code: api.CodeInvalid, Message: err.Error()}
	case err != nil:
		return ticket.Seed{}, source, &api.Error{Code: api.CodeUnavailable, Message: err.Error()}
	}
	if p.ID != "" {
		seed.ID = p.ID
	}
	if p.Title != "" {
		seed.Title = p.Title
	}
	if p.Description != "" {
		seed.Description = p.Description
	}
	return seed, domain.Source{Type: typ, Key: ref}, nil
}

func (c *Creator) write(dir string, t domain.Ticket, seed ticket.Seed, yamlDoc []byte) error {
	data := struct {
		Title, Description, URL, Service string
	}{seed.Title, strings.TrimSpace(seed.Description), seed.URL, t.Services[0].Name}

	files := map[string][]byte{"ticket.yaml": yamlDoc}
	for _, name := range []string{"plan.md", "tasks.md", "manual-tests.md"} {
		var buf bytes.Buffer
		tmpl, err := template.New(name).Delims("[[", "]]").ParseFS(templates, "templates/"+name+".tmpl")
		if err != nil {
			return err
		}
		if err := tmpl.ExecuteTemplate(&buf, name+".tmpl", data); err != nil {
			return err
		}
		files[name] = buf.Bytes()
	}
	if len(t.Services) >= 2 { // the contract between services only exists when there are several
		files["contract.md"] = []byte(contract.Template(t.ID))
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return err
		}
	}

	initial := state.State{TicketID: t.ID, Services: map[string]state.ServiceState{}}
	if len(t.Services) >= 2 {
		initial.Contract.AgreedHash = contract.Parse(contract.Template(t.ID)).AgreedHash()
	}
	for _, s := range t.Services {
		initial.Services[s.Name] = state.ServiceState{Branch: s.Branch, Base: s.Base}
	}
	return c.opts.States.Create(filepath.Join(dir, "state.json"), initial)
}
