# WhatsApp Go Bot

Este repositório traz um bot de WhatsApp escrito em Go, usando [WhatsMeow](https://github.com/tulir/whatsmeow) para a conexão e a API da OpenAI para gerar texto, transcrição de áudio e imagens.

---

## 📋 Pré-requisitos

1. **Docker & Docker Compose**  
2. **Go ≥ 1.23** (opcional, apenas se quiser validar ou compilar localmente)
3. Variáveis de ambiente:
   - `OPENAI_API_KEY` – sua chave da OpenAI  
   - `USER_PHONE` – seu número no formato internacional (ex.: `5561999999999`)  
   - `GROUPS` – lista de `<chatJID>`s de grupos autorizados, separados por vírgula 
   - `PROMPT` – prompt base para `!resumo`  
   - `CHATGPT_PROMPT` – prompt base para `!chatgpt`  
   - `MODEL` – modelo inicial (ex.: `gpt-4o-mini`)
   - `TZ` – fuso horário (ex.: `America/Sao_Paulo`)
- `INSTA_COOKIES_PATH` – caminho para o arquivo de cookies do Instagram/Threads (opcional).
  Monte o arquivo no container e informe o caminho aqui para que o `yt-dlp` consiga autenticar, inclusive para links do threads.com.
   - `TIKTOK_COOKIES_PATH` – arquivo de cookies do TikTok (opcional, uso similar ao do Instagram).

---

## ⚙️ Instalação e setup

1. Clone este repositório e acesse a pasta:
```bash
git clone https://github.com/atcasanova/whatsappBot.git
cd whatsappBot
```

2. (Opcional) Gere e valide seu go.mod e go.sum antes do Docker:

```bash
go mod tidy
go mod download
```

3. (Opcional) Compile localmente para testar:

```bash
go build -o bot .
./bot --help   # ou ./bot para ver logs iniciais
```
4. Configure as variáveis de ambiente, seja criando um arquivo .env com:

```dotenv
OPENAI_API_KEY=sk-...
USER_PHONE=5561999999999
GROUPS=551199999999-11111118@g.us,5511999999999-222222222@g.us
PROMPT="Resuma as mensagens do dia:"
CHATGPT_PROMPT="Responda ao texto a seguir:"
MODEL=gpt-4o-mini
TZ=America/Sao_Paulo
INSTA_COOKIES_PATH=/cookies/insta_cookies.txt
TIKTOK_COOKIES_PATH=/cookies/tiktok_cookies.txt
```
Se já possuir um arquivo de cookies do Instagram, monte-o no container, por exemplo:

```yaml
volumes:
  # remova `:ro` se quiser atualizar os cookies via `!insta`
  - ./insta_cookies.txt:/cookies/insta_cookies.txt
  - ./tiktok_cookies.txt:/cookies/tiktok_cookies.txt
```
Ou diretamente no `docker-compose.yml`

## 🚀 Executando com Docker

```bash
docker-compose down
docker-compose up --build -d
```
* No primeiro start é mostrado na tela um conteúdo em base64, como um qrcode. Para escaneá-lo direto do terminal, execute `qrencode -t ANSI256 <string>`
* A sessão (datastore.db em volume) será criada e reutilizada.

## 💬 Comandos suportados

| Gatilho                     | Onde                           | Exemplo                                  | O que faz                                                           |
|-----------------------------|--------------------------------|------------------------------------------|---------------------------------------------------------------------|
| `!model`                    | Sua conversa consigo mesmo     | `!model`                                 | Mostra o modelo atual usado pelo bot na API OpenAI                  |
| `!model <nome>`             | Sua conversa consigo mesmo     | `!model gpt-4`                           | Altera o modelo do ChatGPT sem reiniciar o container                |
| `!logs <groupJID>`          | Sua conversa consigo mesmo     | `!logs 551199999999-14700@g.us`          | Mostra o histórico armazenado do grupo informado                    |
| `!grupos`                   | Sua conversa consigo mesmo     | `!grupos`                                | Mostra os grupos monitorados para !resumo                           |
| `!grupos add <id do grupo>` | Sua conversa consigo mesmo     | `!grupos add 551199999999-14700@g.us`    | Adiciona grupos monitorados para !resumo                            |
| `!grupos del <id do grupo>` | Sua conversa consigo mesmo     | `!grupos del 551199999999-14700@g.us`    | Remove grupos monitorados para !resumo                              |
| `!insta <cookies>`          | Sua conversa consigo mesmo     | `!insta sessionid=abc`                   | Atualiza o conteúdo do arquivo de cookies usado pelo `yt-dlp`. Também é possível montar o arquivo e definir `INSTA_COOKIES_PATH`. |
| `!tiktok <cookies>`         | Sua conversa consigo mesmo     | `!tiktok sid_tt=abc`                     | Atualiza os cookies do TikTok usados no `yt-dlp`. Também é possível montar o arquivo e definir `TIKTOK_COOKIES_PATH`. |
| `!carteirinha`              | Qualquer conversa              | `!carteirinha`                           | Envia a imagem `carteirinha.jpg` para o chat                        |
| `!cnh`                      | Qualquer conversa              | `!cnh`                                   | Envia o PDF `cnh.pdf` para o chat                                    |
| `!pix`                      | Qualquer conversa              | `!pix 120,50`                            | Gera um payload Pix (com valor opcional)                             |
| `!copia`                    | Qualquer conversa              | (Responder a uma mensagem)               | Extrai e envia e-mails/telefones da mensagem citada                 |
| `!chatgpt <txt>`            | Qualquer conversa              | `!chatgpt Está correto?`                 | Envia `<txt>` + mensagem citada (texto **ou imagem**) ao ChatGPT e devolve a resposta |
| `!edit <prompt>`            | Qualquer conversa              | (Responder a uma imagem)                 | Edita a imagem citada via gpt-image-1.5 e envia o resultado         |
| `!img <prompt>`             | Qualquer conversa              | `!img gato astronauta, estilo cartoon`   | Gera e envia uma imagem via gpt-image-1.5 diretamente no chat       |
| `!sticker`                  | Qualquer conversa              | (Responder a uma imagem ou vídeo curto)  | Converte a mídia citada em figurinha (imagem estática ou animada)   |
| `!download`                 | Sua conversa consigo mesmo     | (Responder a um link)                    | Baixa mídias suportadas pelo `yt-dlp` e envia no chat               |
| `!paywall`                  | Sua conversa consigo mesmo     | (Responder a um link)                    | Gera link alternativo usando `PAYWALL_REMOVER`                      |
| `!ler`                      | Qualquer conversa              | `!ler`                                   | Transcreve o último áudio citado usando Whisper                     |
| `!podcast`                  | Qualquer conversa              | `!podcast`                               | Transcreve o áudio citado e resume com clareza usando o modelo configurado |
| `!resumo`                   | Grupos autorizados             | `!resumo`                                | Gera resumo das mensagens trocadas **hoje** nesse grupo             |

---

## 🛠️ Debug & logs

- **Nível de log**: ajuste em `main.go` na criação do client:
  ```go
  clientLog := waLog.Stdout("Client", "DEBUG", true)
  ```

## Estrutura
```go
.
├── Dockerfile
├── docker-compose.yml
├── go.mod
├── go.sum
├── main.go
└── README.md
```
* Dockerfile: instala Go 1.23, habilita cgo, instala o `yt-dlp` atualizado via `pip`, configura timezone e compila o binário.
* docker-compose.yml: monta volumes para sessão e arquivos de áudio.
* main.go: toda a lógica do bot e handlers de eventos.
