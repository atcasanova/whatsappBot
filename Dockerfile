# Dockerfile
FROM golang:1.24-alpine

RUN apk add --no-cache \
      build-base \
      sqlite-dev \
      tzdata \
      python3 \
      py3-pip

# Use an isolated virtual environment to install yt-dlp and avoid PEP 668
# externally-managed-environment errors during image build.
RUN python3 -m venv /venv \
    && /venv/bin/pip install --no-cache-dir --upgrade yt-dlp \
    && ln -s /venv/bin/yt-dlp /usr/local/bin/yt-dlp
ENV TZ=America/Sao_Paulo

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bot .

CMD ["./bot"]
