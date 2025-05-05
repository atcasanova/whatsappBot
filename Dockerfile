# Dockerfile
FROM golang:1.24-alpine

RUN apk add --no-cache \
      build-base \
      sqlite-dev \
      tzdata
ENV TZ=America/Sao_Paulo

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bot .

CMD ["./bot"]
