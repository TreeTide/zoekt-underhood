FROM golang:1.17-alpine AS builder

WORKDIR /work

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY ./cmd ./cmd
COPY ./web ./web
RUN go build -o /main cmd/zoekt-underhood/main.go

FROM golang:1.17-alpine
COPY --from=builder /main /main

EXPOSE 6080

ENTRYPOINT [ "/main" ]
