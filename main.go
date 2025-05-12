// main.go
package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "mime"
    "net/http"
    "os"
    "os/signal"
    "path"
    "strings"
    "syscall"
    "time"

    "github.com/joho/godotenv"
    go_openai "github.com/sashabaranov/go-openai"
    _ "github.com/mattn/go-sqlite3"

    "go.mau.fi/whatsmeow"
    "go.mau.fi/whatsmeow/store/sqlstore"
    "go.mau.fi/whatsmeow/types/events"
    waProto "go.mau.fi/whatsmeow/proto/waE2E"
    waLog "go.mau.fi/whatsmeow/util/log"
    "go.mau.fi/whatsmeow/types"
    "google.golang.org/protobuf/proto"
)

// Msg representa uma mensagem armazenada, possivelmente com quote
type Msg struct {
    From       string
    Body       string
    Timestamp  time.Time
    QuotedFrom string // bare JID de quem foi citado
    QuotedBody string // texto citado
}

var (
    openaiClient   *go_openai.Client
    model          string
    promptSummary  string
    promptChatGPT  string
    pathMp3        string
    userJID        string
    allowedGroups  map[string]bool
    messageHistory = make(map[string][]Msg)
    currentDay     = time.Now().Day()
)

// bareJID remove o sufixo :agent de um full JID
func bareJID(full string) string {
    parts := strings.SplitN(full, "@", 2)
    local := strings.SplitN(parts[0], ":", 2)[0]
    return local + "@" + parts[1]
}

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
    _ = godotenv.Load()
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
    allowedGroups = make(map[string]bool)
    for _, g := range strings.Split(mustEnv("GROUPS", ""), ",") {
        if g != "" {
            allowedGroups[g] = true
        }
    }
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
    client.AddEventHandler(func(evt interface{}) {
        switch v := evt.(type) {
        case *events.LoggedOut:
            log.Println("⚠️ logout remoto, limpando sessão e reiniciando...")
            os.RemoveAll(sessionPath)
            os.Exit(1)
        case *events.Message:
            handleMessage(client, v)
        }
    })
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
    senderBare := bareJID(v.Info.Sender.String())
    chatBare := bareJID(v.Info.Chat.String())
    fromName := v.Info.PushName
    if fromName == "" {
        fromName = senderBare
    }
    log.Printf("📥 DEBUG sender=%s chat=%s body=%q", senderBare, chatBare, body)
    // Reset diário
    if time.Now().Day() != currentDay {
        messageHistory = make(map[string][]Msg)
        currentDay = time.Now().Day()
    }
    // Armazena histórico de grupos
    if _, ok := allowedGroups[chatBare]; ok {
        var qf, qb string
        if ext := v.Message.GetExtendedTextMessage(); ext != nil {
            ctx := ext.GetContextInfo()
            if ctx != nil && ctx.GetQuotedMessage() != nil {
                qb = ctx.GetQuotedMessage().GetConversation()
                qf = bareJID(ctx.GetParticipant())
            }
        }
        messageHistory[chatBare] = append(messageHistory[chatBare], Msg{
            From:       fromName,
            Body:       body,
            Timestamp:  v.Info.Timestamp,
            QuotedFrom: qf,
            QuotedBody: qb,
        })
    }
    // !img
    if (allowedGroups[chatBare] || chatBare == userJID) && strings.HasPrefix(body, "!img ") {
        prompt := strings.TrimSpace(body[5:])
        log.Printf("🖼️ Gerando imagem para: %q", prompt)
        imgResp, err := openaiClient.CreateImage(
            context.Background(),
            go_openai.ImageRequest{
                Prompt:  prompt,
                N:       1,
                Size:    go_openai.CreateImageSize1024x1024,
                Model:   go_openai.CreateImageModelDallE3,
                Quality: go_openai.CreateImageQualityStandard,
            },
        )
        if err != nil {
            sendText(cli, chatBare, "❌ Erro ao gerar imagem: "+err.Error())
            return
        }
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
        uploadResp, err := cli.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
        if err != nil {
            sendText(cli, chatBare, "❌ Erro no upload da imagem: "+err.Error())
            return
        }
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
            FileSHA256:    uploadResp.FileSHA256,
            FileLength:    proto.Uint64(uploadResp.FileLength),
        }
        if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imageMsg}); err != nil {
            log.Printf("❌ falha ao enviar imagem: %v", err)
        }
        return
    }
    // !grupos
    if chatBare == userJID && strings.HasPrefix(body, "!grupos") {
        parts := strings.Fields(body)
        switch {
        case len(parts) == 1:
            // lista grupos
            var list []string
            for g := range allowedGroups {
                list = append(list, g)
            }
            if len(list) == 0 {
                sendText(cli, chatBare, "Nenhum grupo autorizado.")
            } else {
                sendText(cli, chatBare, "Grupos autorizados: "+strings.Join(list, ", "))
            }
        case len(parts) == 3 && parts[1] == "add":
            gid := parts[2]
            allowedGroups[gid] = true
            sendText(cli, chatBare, fmt.Sprintf("✅ Grupo %s adicionado.", gid))
        case len(parts) == 3 && parts[1] == "del":
            gid := parts[2]
            delete(allowedGroups, gid)
            sendText(cli, chatBare, fmt.Sprintf("✅ Grupo %s removido.", gid))
        default:
            sendText(cli, chatBare, "Uso: !grupos [add|del] <chatJID>")
        }
        return
    }
    // !model
    if chatBare == userJID && strings.HasPrefix(body, "!model") {
        if body == "!model" {
            sendText(cli, chatBare, fmt.Sprintf("Modelo atual: %s", model))
        } else if strings.HasPrefix(body, "!model ") {
            newModel := strings.TrimSpace(body[7:])
            model = newModel
            sendText(cli, chatBare, fmt.Sprintf("✅ Modelo alterado para %s", model))
        }
        return
    }
    // !ler
    if chatBare == userJID && body == "!ler" {
        log.Println("✅ Disparou !ler")
        if ext := v.Message.GetExtendedTextMessage(); ext != nil {
            ctx := ext.GetContextInfo()
            if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
                aud := qm.GetAudioMessage()
                exts, _ := mime.ExtensionsByType(aud.GetMimetype())
                var ext string
                if len(exts) > 0 {
                    ext = exts[0]
                } else {
                    ext = ".ogg"
                }
                origID := ctx.GetStanzaId()
                filePath := path.Join(pathMp3, origID+ext)
                resp, err := openaiClient.CreateTranscription(context.Background(), go_openai.AudioRequest{Model: go_openai.Whisper1, FilePath: filePath})
                if err != nil {
                    sendText(cli, chatBare, "❌ Erro na transcrição: "+err.Error())
                    return
                }
                sendText(cli, chatBare, "🗣️ "+resp.Text)
            }
        }
        return
    }
    // !resumo
    if allowedGroups[chatBare] && body == "!resumo" {
        log.Println("✅ Disparou !resumo")
        hoje := time.Now().Truncate(24 * time.Hour)
        msgs := messageHistory[chatBare]
        var sb strings.Builder
        for _, m := range msgs {
            if m.Timestamp.Truncate(24 * time.Hour).Equal(hoje) {
                if m.QuotedFrom != "" {
                    sb.WriteString(fmt.Sprintf("%s — %s em resposta a %s, que disse '%s': %s\n",
                        m.Timestamp.Format("15:04"), m.From, m.QuotedFrom, m.QuotedBody, m.Body))
                } else {
                    sb.WriteString(fmt.Sprintf("%s — %s: %s\n",
                        m.Timestamp.Format("15:04"), m.From, m.Body))
                }
            }
        }
        if sb.Len() > 0 {
            req := go_openai.ChatCompletionRequest{Model: model, Messages: []go_openai.ChatCompletionMessage{{Role: go_openai.ChatMessageRoleUser, Content: promptSummary + "\n\n" + sb.String()}}}
            if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
                sendText(cli, chatBare, "📋 Resumo:\n"+resp.Choices[0].Message.Content)
            }
        }
        return
    }
    // !chatgpt
    if (allowedGroups[chatBare] || chatBare == userJID) && strings.HasPrefix(body, "!chatgpt") {
        log.Println("✅ Disparou !chatgpt")
        ext := v.Message.GetExtendedTextMessage()
        userMsg := strings.TrimSpace(body[len("!chatgpt"):])
        var quotedText string
        if ext != nil {
            ctx := ext.GetContextInfo()
            if ctx != nil && ctx.GetQuotedMessage() != nil {
                qm := ctx.GetQuotedMessage()
                quotedText = qm.GetConversation()
                if quotedText == "" && qm.GetExtendedTextMessage() != nil {
                    quotedText = qm.GetExtendedTextMessage().GetText()
                }
            }
        }
        finalPrompt := userMsg
        if quotedText != "" {
            finalPrompt = fmt.Sprintf("%s\n\nMensagem citada: %s", userMsg, quotedText)
        }
        if finalPrompt != "" {
            req := go_openai.ChatCompletionRequest{Model: model, Messages: []go_openai.ChatCompletionMessage{{Role: go_openai.ChatMessageRoleUser, Content: promptChatGPT + "\n\n" + finalPrompt}}}
            if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
                sendText(cli, chatBare, resp.Choices[0].Message.Content)
            }
        }
    }
}

func sendText(cli *whatsmeow.Client, to, text string) {
    jid, err := types.ParseJID(to)
    if err != nil {
        log.Printf("⚠️  JID inválido %q: %v", to, err)
        return
    }
    msg := &waProto.Message{Conversation: proto.String(text)}
    if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
        log.Printf("❌ falha ao enviar mensagem: %v", err)
    }
}

func main() {}
