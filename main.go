// main.go
package main

import (
   "context"
   "fmt"
   "log"
   "os"
   "os/signal"
   "syscall"
   "time"
   "mime"
   "path"
   "strings"
   "net/http"
   "io"

   "github.com/joho/godotenv"
   go_openai "github.com/sashabaranov/go-openai"
   _ "github.com/mattn/go-sqlite3"

   "go.mau.fi/whatsmeow"
   "go.mau.fi/whatsmeow/store/sqlstore"
   "go.mau.fi/whatsmeow/types/events"               // eventos do WhatsMeow :contentReference[oaicite:1]{index=1}
   waProto "go.mau.fi/whatsmeow/proto/waE2E"
   waLog "go.mau.fi/whatsmeow/util/log"
   "go.mau.fi/whatsmeow/types"
   "google.golang.org/protobuf/proto"
)

// Mensagem histórica para cada grupo
type Msg struct {
    From      string
    Body      string
    Timestamp time.Time
}

var (
    openaiClient     *go_openai.Client
    model            string
    promptSummary    string
    promptChatGPT    string
    pathMp3          string
    userJID          string
    allowedGroups    map[string]bool
    messageHistory   = make(map[string][]Msg)
    currentDay       = time.Now().Day()
)

func mustEnv(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}

func init() {
    if tz := os.Getenv("TZ"); tz != "" {
        if loc, err := time.LoadLocation(tz); err != nil {
            log.Printf("⚠️  TZ inválido %q: %v", tz, err)
        } else {
            time.Local = loc
            log.Printf("⏰ timezone setado para %s", loc)
        }
    }
    // Carrega .env se existir
    _ = godotenv.Load()
    // Variáveis de ambiente
    openaiClient = go_openai.NewClient(os.Getenv("OPENAI_API_KEY"))
    pathMp3 = mustEnv("PATH_MP3", ".")
    sessionPath := mustEnv("PATH_SESSION", "./")
    userPhone := mustEnv("USER_PHONE", "")
    if userPhone == "" {
        log.Fatal("USER_PHONE não definido")
    }
    userJID = userPhone + "@s.whatsapp.net"
    model = mustEnv("MODEL", "gpt-4o-mini")
    promptSummary = mustEnv("PROMPT", "Faça um resumo das seguintes mensagens...")
    promptChatGPT = mustEnv("CHATGPT_PROMPT", "Responda ao questionamento a seguir...")
    // Grupos permitidos
    allowedGroups = make(map[string]bool)
    for _, g := range strings.Split(mustEnv("GROUPS", ""), ",") {
        allowedGroups[g] = true
    }
    // Configura Whatsmeow com SQLite no diretório de sessão :contentReference[oaicite:0]{index=0}
    dbLog := waLog.Stdout("DB", "ERROR", true)
    dsn := fmt.Sprintf("file:%s/datastore.db?_foreign_keys=on", sessionPath)
    sqlContainer, err := sqlstore.New("sqlite3", dsn, dbLog)
    if err != nil {
        log.Fatalf("erro ao abrir store: %v", err)
    }
    deviceStore, err := sqlContainer.GetFirstDevice()
    if err != nil {
        log.Fatalf("erro ao obter device store: %v", err)
    }
    clientLog := waLog.Stdout("Client", "ERROR", true)
    client := whatsmeow.NewClient(deviceStore, clientLog)

    // Conecta (QR ao primeiro login)
    if client.Store.ID == nil {
        qrChan, _ := client.GetQRChannel(context.Background())
        go func() {
            for evt := range qrChan {
                fmt.Println("QR Code:", evt.Code)
            }
        }()
    }
    if err := client.Connect(); err != nil {
        log.Fatalf("falha ao conectar: %v", err)
    }

    // Handler de eventos
    client.AddEventHandler(func(evt interface{}) {
        switch v := evt.(type) {
        case *events.Message:
            handleMessage(client, v)
        }
    })

    // Mantém o programa vivo até Ctrl+C
    c := make(chan os.Signal, 1)
    signal.Notify(c, os.Interrupt, syscall.SIGTERM)
    <-c
    client.Disconnect()
}

func handleMessage(cli *whatsmeow.Client, v *events.Message) {
   body := v.Message.GetConversation()
   if ext := v.Message.GetExtendedTextMessage(); ext != nil {
       body = ext.GetText()
   }
   // monta o nome de exibição
   fullSender := v.Info.Sender.String()
   parts := strings.SplitN(fullSender, "@", 2)
   local := parts[0]
   domain := parts[1]
   localBare := strings.SplitN(local, ":", 2)[0]
   senderBare := fmt.Sprintf("%s@%s", localBare, domain)
   fullChat := v.Info.Chat.String()
   parts = strings.SplitN(fullChat, "@", 2)
   local = parts[0]
   domain = parts[1]
   localBare = strings.SplitN(local, ":", 2)[0]
   chatBare := fmt.Sprintf("%s@%s", localBare, domain)

   fromName := v.Info.PushName
   if fromName == "" {
       fromName = senderBare
   }
   
   chatJID := chatBare
   senderJID := senderBare

   log.Printf("📥 Evento Message: DEBUG senderJID=%s from=%s chat=%s body=%q",
       senderJID, fromName, v.Info.Chat, body)


    // Reset diário
    if time.Now().Day() != currentDay {
        messageHistory = make(map[string][]Msg)
        currentDay = time.Now().Day()
    }

    // Salvando histórico de grupo
    if _, ok := allowedGroups[chatJID]; ok {
        fromName := v.Info.PushName
        if fromName == "" {
          fromName = senderJID
        }
        messageHistory[chatJID] = append(messageHistory[chatJID], Msg{
            From:      fromName,
            Body:      body,
            Timestamp: v.Info.Timestamp,
        })
    }

    // Download de áudios (voz) automaticamente :contentReference[oaicite:1]{index=1}
   if aud := v.Message.GetAudioMessage(); aud != nil {
       data, err := cli.Download(aud)
       if err == nil {
           // tenta descobrir extensão; se não achar, cai em .ogg
           exts, _ := mime.ExtensionsByType(aud.GetMimetype())
           var ext string
           if len(exts) > 0 {
               ext = exts[0]
           } else {
               ext = ".ogg"
           }
           fn := path.Join(pathMp3, v.Info.ID+ext)
           _ = os.WriteFile(fn, data, 0644)
       }
   }

// !img: gerar imagem DALL·E 3 Standard e enviar como mídia
   if senderJID == userJID && strings.HasPrefix(body, "!img ") {
       prompt := strings.TrimSpace(body[len("!img "):])
       log.Printf("🖼️ Gerando imagem para: %q", prompt)

       // 1) Chama a API de imagens (DALL·E 3 Standard, 1024×1024)
       imgResp, err := openaiClient.CreateImage(
           context.Background(),
           go_openai.ImageRequest{
               Prompt:  prompt,
               N:       1,
               Size:    go_openai.CreateImageSize1024x1024,        // :contentReference[oaicite:0]{index=0}
               Model:   go_openai.CreateImageModelDallE3,          // :contentReference[oaicite:1]{index=1}
               Quality: go_openai.CreateImageQualityStandard,     // :contentReference[oaicite:2]{index=2}
           },
       )
    if err != nil {
        sendText(cli, chatBare, "❌ Erro ao gerar imagem: "+err.Error())
        return
    }

    // 2) Faz o download do arquivo gerado
    url := imgResp.Data[0].URL
    httpResp, err := http.Get(url)
    if err != nil {
        sendText(cli, chatBare, "❌ Falha ao baixar imagem: "+err.Error())
        return
    }
    defer httpResp.Body.Close()
    imgBytes, err := io.ReadAll(httpResp.Body)
    if err != nil {
        sendText(cli, chatBare, "❌ Não consegui ler a imagem: "+err.Error())
        return
    }

    // 3) Envia para o WhatsApp via upload
    uploadResp, err := cli.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
    if err != nil {
        sendText(cli, chatBare, "❌ Erro no upload da imagem: "+err.Error())
        return
    }
       // 4) Monta e envia a ImageMessage
       jid, err := types.ParseJID(chatBare)
       if err != nil {
           log.Printf("⚠️ JID inválido para imagem: %v", err)
           return
       }
       imageMsg := &waProto.ImageMessage{
           Caption:       proto.String(prompt),
           Mimetype:      proto.String(httpResp.Header.Get("Content-Type")),
           URL:           proto.String(uploadResp.URL),
           DirectPath:    proto.String(uploadResp.DirectPath),
           MediaKey:      uploadResp.MediaKey,
           FileEncSHA256: uploadResp.FileEncSHA256,
           FileSHA256:    uploadResp.FileSHA256,  // campo correto
           FileLength:    proto.Uint64(uploadResp.FileLength),
       }
       if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{
           ImageMessage: imageMsg,
       }); err != nil {
           log.Printf("❌ falha ao enviar imagem: %v", err)
       }
       return
}


    // !model: só eu para mim mesmo (chat privado)
    if senderJID == userJID && chatJID == userJID && strings.HasPrefix(body, "!model ") {
        log.Printf("✅ Disparou !model")
        newModel := strings.TrimSpace(body[len("!model "):])
        model = newModel
        sendText(cli, chatJID, fmt.Sprintf("✅ Modelo alterado para *%s*", model))
        return
    }

// !ler: transcrever áudio citado
if senderJID == userJID && body == "!ler" {
    log.Println("✅ Disparou !ler")
    if ext := v.Message.GetExtendedTextMessage(); ext != nil {
        ctx := ext.GetContextInfo()
        if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
            aud := qm.GetAudioMessage()
            // pega extensão (ou .ogg por default)
            exts, _ := mime.ExtensionsByType(aud.GetMimetype())
            var ext string
            if len(exts) > 0 {
                ext = exts[0]
            } else {
                ext = ".ogg"
            }
            origID := ctx.GetStanzaId()
            filePath := path.Join(pathMp3, origID+ext) // ex: "/mp3/XYZ.ogg"

            // agora só passamos o caminho pro go-openai
            resp, err := openaiClient.CreateTranscription(
                context.Background(),
                go_openai.AudioRequest{
                    Model:    go_openai.Whisper1,
                    FilePath: filePath,            // ← aqui
                },
            )
            if err != nil {
                sendText(cli, chatJID, "❌ Erro na transcrição: "+err.Error())
                return
            }
            sendText(cli, chatJID, "🗣️ "+resp.Text)
        }
    }
    return
}

    // !resumo: resumo do dia no grupo
    if _, ok := allowedGroups[chatJID]; ok && body == "!resumo" {
        log.Println("✅ Disparou !resumo")
        hoje := time.Now().Truncate(24 * time.Hour)
        msgs := messageHistory[chatJID]
        var sb strings.Builder
        for _, m := range msgs {
            if m.Timestamp.Truncate(24 * time.Hour).Equal(hoje) {
                sb.WriteString(fmt.Sprintf("%s — %s: %s\n",
                    m.Timestamp.Format("15:04"), m.From, m.Body))
            }
        }
        if sb.Len() > 0 {
            req := go_openai.ChatCompletionRequest{
                Model: model,
                Messages: []go_openai.ChatCompletionMessage{
                    {Role: go_openai.ChatMessageRoleUser, Content: promptSummary + "\n\n" + sb.String()},
                },
            }
            if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
                sendText(cli, chatJID, "📋 Resumo:\n"+resp.Choices[0].Message.Content)
            }
        }
        return
    }

    // !chatgpt: interação direta incluindo mensagem citada
    if (allowedGroups[chatJID] || senderJID == userJID) && strings.HasPrefix(body, "!chatgpt") {
        log.Println("✅ Disparou !chatgpt")
        ext := v.Message.GetExtendedTextMessage()
        // texto que vem depois de "!chatgpt"
        userMsg := strings.TrimSpace(body[len("!chatgpt"):])

        // tenta extrair a mensagem citada
        var quotedText string
        if ext != nil {
            ctx := ext.GetContextInfo()
            if ctx != nil && ctx.GetQuotedMessage() != nil {
                qm := ctx.GetQuotedMessage()
                // primeiro tenta o campo Conversation
                quotedText = qm.GetConversation()
                // se for ExtendedTextMessage, pega o texto dele
                if quotedText == "" && qm.GetExtendedTextMessage() != nil {
                    quotedText = qm.GetExtendedTextMessage().GetText()
                }
            }
        }

        // monta o prompt final: "usuário diz: ..." se houver citação
        finalPrompt := userMsg
        if quotedText != "" {
            finalPrompt = fmt.Sprintf("%s\n\nMensagem citada: %s", userMsg, quotedText)
        }

        // envia à API
        if finalPrompt != "" {
            req := go_openai.ChatCompletionRequest{
                Model: model,
                Messages: []go_openai.ChatCompletionMessage{
                    {Role: go_openai.ChatMessageRoleUser, Content: promptChatGPT + "\n\n" + finalPrompt},
                },
            }
            if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
                sendText(cli, chatJID, resp.Choices[0].Message.Content)
            }
        }
    }
}

func sendText(cli *whatsmeow.Client, to, text string) {
    // Converte string para types.JID
    jid, err := types.ParseJID(to)
    if err != nil {
        log.Printf("⚠️  JID inválido %q: %v", to, err)
        return
    }
    msg := &waProto.Message{
        Conversation: proto.String(text),
    }
    // SendMessage aceita varargs de SendRequestExtra; aqui não passamos nenhum
    if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
        log.Printf("❌ falha ao enviar mensagem: %v", err)
    }
}

// main.go, logo depois dos seus init() e demais funções:
func main() {
    // Vazio porque toda a inicialização e o bloqueio já
    // aconteceram em init().
}
