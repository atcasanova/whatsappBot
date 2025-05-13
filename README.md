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
| `!ler`                      | Qualquer conversa              | `!ler`                                   | Transcreve o último áudio citado usando Whisper                     |
| `!resumo`                   | Grupos autorizados             | `!resumo`                                | Gera resumo das mensagens trocadas **hoje** nesse grupo             |
| `!grupos`                   | Sua conversa consigo mesmo     | `!grupos`                                | Mostra os grupos monitorados para !resumo                           |
| `!grupos add <id do grupo>` | Sua conversa consigo mesmo     | `!grupos add 551199999999-14700@g.us`    | Adiciona grupos monitorados para !resumo                            |
| `!grupos del <id do grupo>` | Sua conversa consigo mesmo     | `!grupos del 551199999999-14700@g.us`    | Remove grupos monitorados para !resumo                              |
| `!chatgpt <txt>`            | Qualquer conversa              | `!chatgpt Está correto?`                 | Envia `<txt>` + mensagem citada ao ChatGPT e devolve a resposta     |
| `!img <prompt>`             | Qualquer conversa              | `!img gato astronauta, estilo cartoon`   | Gera e envia uma imagem via DALL·E 3 (Standard) diretamente no chat |

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
* Dockerfile: instala Go 1.23, habilita cgo, configura timezone e compila o binário.
* docker-compose.yml: monta volumes para sessão e arquivos de áudio.
* main.go: toda a lógica do bot e handlers de eventos.
