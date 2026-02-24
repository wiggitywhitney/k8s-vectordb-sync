FROM golang:1.26 AS builder
WORKDIR /app
RUN go mod init mockserver
COPY main.go .
RUN CGO_ENABLED=0 go build -o mock-server .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/mock-server /mock-server
USER 65532:65532
ENTRYPOINT ["/mock-server"]
