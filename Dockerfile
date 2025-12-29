# Dockerfile
FROM golang:1.24-alpine

RUN apk add --no-cache \
      build-base \
      python3 \
      sqlite-dev \
      tzdata \
      wget \
      ffmpeg
RUN wget https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -O /usr/bin/yt-dlp \
    && chmod +x /usr/bin/yt-dlp
ENV TZ=America/Sao_Paulo

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bot .

CMD ["./bot"]
