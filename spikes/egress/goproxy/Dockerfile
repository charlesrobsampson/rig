FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY main.go .
RUN go mod init egressproxy && CGO_ENABLED=0 go build -ldflags "-s -w" -o /goproxy .

FROM scratch
COPY --from=build /goproxy /goproxy
ENTRYPOINT ["/goproxy"]
