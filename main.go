package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	go_openai "github.com/sashabaranov/go-openai"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const summaryMarker = "📋󠅢󠅕󠅣󠅥󠅝󠅟"

// Msg representa uma mensagem armazenada, possivelmente com quote
type Msg struct {
	From       string
	Body       string
	Timestamp  time.Time
	QuotedFrom string // nome + bare JID de quem foi citado
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
	instaCookies   string
	tiktokCookies  string
	messageHistory = make(map[string][]Msg)
	currentDay     = time.Now().Day()
	contactNames   = make(map[string]string)
)

// helpers de contexto
type void struct{}

func isFromMe(sender string) bool {
	return sender == userJID
}

func isPrivateChat(chat string) bool {
	return chat == userJID
}

func isAuthorizedGroup(chat string) bool {
	return allowedGroups[chat]
}

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

var reVideoURL = regexp.MustCompile(`https?://[^\s]*?(instagram\.com|tiktok\.com|vm\.tiktok\.com|vt\.tiktok\.com|youtube\.com|youtu\.be|x\.com|twitter\.com)[^\s]*`)

func extractVideoURL(text string) string {
	match := reVideoURL.FindString(text)
	return match
}

func downloadAndSendMedia(cli *whatsmeow.Client, chat string, url string) {
	tmpDir, err := os.MkdirTemp("", "vid-*")
	if err != nil {
		log.Printf("erro temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	args := []string{}
	if strings.Contains(url, "instagram.com") && instaCookies != "" {
		if _, err := os.Stat(instaCookies); err == nil {
			args = append(args, "--cookies", instaCookies)
		} else {
			log.Printf("⚠️ cookies file not found %s: %v", instaCookies, err)
		}
	}
	if strings.Contains(url, "tiktok.com") && tiktokCookies != "" {
		if _, err := os.Stat(tiktokCookies); err == nil {
			args = append(args, "--cookies", tiktokCookies)
		} else {
			log.Printf("⚠️ cookies file not found %s: %v", tiktokCookies, err)
		}
	}
	tmpl := path.Join(tmpDir, "%(id)s.%(ext)s")
	args = append(args, "-o", tmpl, url)
	log.Printf("▶️ yt-dlp %v", args)
	cmd := exec.Command("yt-dlp", args...)
	out, err := cmd.CombinedOutput()
	log.Printf("yt-dlp output: %s", string(out))
	if err != nil {
		log.Printf("yt-dlp erro: %v", err)
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		log.Printf("erro listando diretório temporário: %v", err)
		return
	}
	if len(files) == 0 {
		log.Printf("⚠️ nenhum arquivo baixado")
		return
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		log.Printf("jid inválido: %v", err)
		return
	}
	for _, f := range files {
		fp := path.Join(tmpDir, f.Name())
		data, err := os.ReadFile(fp)
		if err != nil {
			log.Printf("erro lendo arquivo %s: %v", f.Name(), err)
			continue
		}
		mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(f.Name())))
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		switch {
		case strings.HasPrefix(mimeType, "video/"):
			up, err := cli.Upload(context.Background(), data, whatsmeow.MediaVideo)
			if err != nil {
				log.Printf("erro upload video: %v", err)
				continue
			}
			vidMsg := &waProto.VideoMessage{
				Mimetype:      proto.String(mimeType),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{VideoMessage: vidMsg}); err != nil {
				log.Printf("erro enviando video: %v", err)
			} else {
				log.Printf("✅ video enviado para %s", chat)
			}
		case strings.HasPrefix(mimeType, "image/"):
			up, err := cli.Upload(context.Background(), data, whatsmeow.MediaImage)
			if err != nil {
				log.Printf("erro upload imagem: %v", err)
				continue
			}
			imgMsg := &waProto.ImageMessage{
				Mimetype:      proto.String(mimeType),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imgMsg}); err != nil {
				log.Printf("erro enviando imagem: %v", err)
			} else {
				log.Printf("✅ imagem enviada para %s", chat)
			}
		default:
			log.Printf("⚠️ tipo de arquivo não suportado: %s", f.Name())
		}
	}
}

func init() {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err != nil {
			log.Printf("⚠️ TZ inválido %q: %v", tz, err)
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
	instaCookies = mustEnv("INSTA_COOKIES_PATH", "./insta_cookies.txt")
	tiktokCookies = mustEnv("TIKTOK_COOKIES_PATH", "./tiktok_cookies.txt")

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
	// 1) extrai texto
	body := v.Message.GetConversation()
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		body = ext.GetText()
	}
	// JIDs
	senderBare := bareJID(v.Info.Sender.String())
	chatBare := bareJID(v.Info.Chat.String())
	senderJID := senderBare

	// atualiza cache de nomes
	fromName := v.Info.PushName
	if fromName == "" {
		fromName = senderBare
	}
	contactNames[senderBare] = fromName

	log.Printf("📥 DEBUG sender=%s chat=%s body=%q", senderBare, chatBare, body)

	// reset diário
	if time.Now().Day() != currentDay {
		messageHistory = make(map[string][]Msg)
		currentDay = time.Now().Day()
	}

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
			log.Println("✅ Baixou Audio")
		}
	}

	// ==== comandos GLOBAIS (qualquer chat) ====
	if isFromMe(senderJID) {
		// !chatgpt
		if strings.HasPrefix(body, "!chatgpt") {
			log.Println("✅ Disparou !chatgpt")
			ext := v.Message.GetExtendedTextMessage()
			userMsg := strings.TrimSpace(body[len("!chatgpt"):])
			var quotedText string
			if ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil {
						quotedText = qm.GetConversation()
						if quotedText == "" && qm.GetExtendedTextMessage() != nil {
							quotedText = qm.GetExtendedTextMessage().GetText()
						}
					}
				}
			}
			prompt := userMsg
			if quotedText != "" {
				prompt = fmt.Sprintf("%s\n\nMensagem citada: %s", userMsg, quotedText)
			}
			if prompt != "" {
				req := go_openai.ChatCompletionRequest{
					Model: model,
					Messages: []go_openai.ChatCompletionMessage{{
						Role:    go_openai.ChatMessageRoleUser,
						Content: promptChatGPT + "\n\n" + prompt,
					}},
				}
				if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
					sendText(cli, chatBare, resp.Choices[0].Message.Content)
				}
			}
			return

		}
		// !img
		if strings.HasPrefix(body, "!img ") {
			prompt := strings.TrimSpace(body[len("!img "):])
			log.Printf("🖼️ Gerando imagem para: %q", prompt)
			respImg, err := openaiClient.CreateImage(
				context.Background(),
				go_openai.ImageRequest{
					Prompt:  prompt,
					N:       1,
					Size:    go_openai.CreateImageSize1024x1024,
					Model:   "gpt-image-1",
					Quality: "high",
				},
			)
			if err != nil {
				sendText(cli, chatBare, "❌ Erro ao gerar imagem: "+err.Error())
				return
			}
			var (
				imgBytes []byte
				mimeType string
			)
			if url := respImg.Data[0].URL; url != "" {
				httpResp, err := http.Get(url)
				if err != nil {
					sendText(cli, chatBare, "❌ Falha ao baixar imagem: "+err.Error())
					return
				}
				defer httpResp.Body.Close()
				imgBytes, err = io.ReadAll(httpResp.Body)
				if err != nil {
					sendText(cli, chatBare, "❌ Não consegui ler a imagem: "+err.Error())
					return
				}
				mimeType = httpResp.Header.Get("Content-Type")
			} else if b64 := respImg.Data[0].B64JSON; b64 != "" {
				imgBytes, err = base64.StdEncoding.DecodeString(b64)
				if err != nil {
					sendText(cli, chatBare, "❌ Não consegui decodificar a imagem: "+err.Error())
					return
				}
				mimeType = "image/png"
			} else {
				sendText(cli, chatBare, "❌ Resposta da API sem imagem")
				return
			}
			up, err := cli.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
			if err != nil {
				sendText(cli, chatBare, "❌ Erro no upload da imagem: "+err.Error())
				return
			}
			jid, err := types.ParseJID(chatBare)
			if err != nil {
				log.Printf("⚠️ JID inválido: %v", err)
				return
			}
			imageMsg := &waProto.ImageMessage{
				Caption:       proto.String(prompt),
				Mimetype:      proto.String(mimeType),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imageMsg}); err != nil {
				log.Printf("❌ falha ao enviar imagem: %v", err)
			}
			return
		}
		if body == "!download" {
			log.Println("✅ Disparou !download")
			var quotedText string
			if ext := v.Message.GetExtendedTextMessage(); ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil {
						quotedText = qm.GetConversation()
						if quotedText == "" && qm.GetExtendedTextMessage() != nil {
							quotedText = qm.GetExtendedTextMessage().GetText()
						}
					}
				}
			}
			if quotedText == "" {
				sendText(cli, chatBare, "❌ Responda ao link para usar !download.")
				return
			}
			url := extractVideoURL(quotedText)
			if url != "" {
				go downloadAndSendMedia(cli, chatBare, url)
			} else {
				sendText(cli, chatBare, "❌ Link inválido para download.")
			}
			return
		}
		// !ler
		if body == "!ler" {
			log.Println("✅ Disparou !ler")
			if ext := v.Message.GetExtendedTextMessage(); ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
						aud := qm.GetAudioMessage()
						exts, _ := mime.ExtensionsByType(aud.GetMimetype())
						ext := ".ogg"
						if len(exts) > 0 {
							ext = exts[0]
						}
						orig := ctx.GetStanzaId()
						filePath := path.Join(pathMp3, orig+ext)
						tr, err := openaiClient.CreateTranscription(
							context.Background(),
							go_openai.AudioRequest{Model: go_openai.Whisper1, FilePath: filePath},
						)
						if err != nil {
							sendText(cli, chatBare, "❌ Erro na transcrição: "+err.Error())
						} else {
							sendText(cli, chatBare, "🗣️ "+tr.Text)
						}
					}
				}
			}
			return
		}
	}

	// ==== comando !resumo (antes de gravar) ====
	if isAuthorizedGroup(chatBare) && isFromMe(senderJID) && body == "!resumo" {
		log.Println("✅ Disparou !resumo")
		logs := messageHistory[chatBare]
		if len(logs) == 0 {
			sendText(cli, chatBare, "❌ Sem mensagens para resumir hoje.")
			return
		}
		var sb strings.Builder
		for _, m := range logs {
			if m.QuotedFrom != "" {
				sb.WriteString(fmt.Sprintf("%s — %s em resposta a %s, que disse '%s': %s\n",
					m.Timestamp.Format("15:04"), m.From, m.QuotedFrom, m.QuotedBody, m.Body))
			} else {
				sb.WriteString(fmt.Sprintf("%s — %s: %s\n\n",
					m.Timestamp.Format("15:04"), m.From, m.Body))
			}
		}
		req := go_openai.ChatCompletionRequest{
			Model: model,
			Messages: []go_openai.ChatCompletionMessage{{
				Role:    go_openai.ChatMessageRoleUser,
				Content: promptSummary + "\n\n" + sb.String(),
			}},
		}
		if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
			sendText(cli, chatBare, summaryMarker+" Resumo:\n"+resp.Choices[0].Message.Content)
		}
		return
	}

	// ==== grava histórico (ignora comandos, resumo e bodies vazios) ====
	if isAuthorizedGroup(chatBare) &&
		strings.TrimSpace(body) != "" &&
		!strings.HasPrefix(body, "!resumo") &&
		!strings.HasPrefix(body, summaryMarker) {

		var qf, qb string
		if ext := v.Message.GetExtendedTextMessage(); ext != nil {
			if ctx := ext.GetContextInfo(); ctx != nil && ctx.GetQuotedMessage() != nil {
				qb = ctx.GetQuotedMessage().GetConversation()
				quoted := bareJID(ctx.GetParticipant())
				if name, ok := contactNames[quoted]; ok {
					qf = fmt.Sprintf("%s (%s)", name, quoted)
				} else {
					qf = quoted
				}
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

	// ==== comandos na MINHA DM (!logs, !model, !grupos) ====
	if isFromMe(senderJID) && isPrivateChat(chatBare) {
		if strings.HasPrefix(body, "!logs ") {
			parts := strings.Fields(body)
			if len(parts) != 2 {
				sendText(cli, chatBare, "Uso: !logs <groupJID>")
			} else {
				gid := parts[1]
				logs := messageHistory[gid]
				if len(logs) == 0 {
					sendText(cli, chatBare, "❌ Sem histórico para o grupo "+gid)
				} else {
					var sb strings.Builder
					for _, m := range logs {
						if m.QuotedFrom != "" {
							sb.WriteString(fmt.Sprintf("%s — %s em resposta a %s: %s\n",
								m.Timestamp.Format("15:04"), m.From, m.QuotedFrom, m.Body))
						} else {
							sb.WriteString(fmt.Sprintf("%s — %s: %s\n",
								m.Timestamp.Format("15:04"), m.From, m.Body))
						}
					}
					sendText(cli, chatBare, sb.String())
				}
			}
			return
		}
		if strings.HasPrefix(body, "!grupos") {
			parts := strings.Fields(body)
			switch {
			case len(parts) == 1:
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
		if body == "!model" {
			sendText(cli, chatBare, fmt.Sprintf("Modelo atual: %s", model))
			return
		}
		if strings.HasPrefix(body, "!model ") {
			newModel := strings.TrimSpace(body[len("!model "):])
			model = newModel
			sendText(cli, chatBare, fmt.Sprintf("✅ Modelo alterado para %s", model))
			return
		}
		if strings.HasPrefix(body, "!insta ") {
			cookies := strings.TrimSpace(body[len("!insta "):])
			if cookies == "" {
				sendText(cli, chatBare, "Uso: !insta <cookies>")
				return
			}
			if err := os.WriteFile(instaCookies, []byte(cookies), 0600); err != nil {
				sendText(cli, chatBare, "❌ Falha ao salvar cookies: "+err.Error())
			} else {
				sendText(cli, chatBare, "✅ Cookies do Instagram atualizados.")
			}
			return
		}
		if strings.HasPrefix(body, "!tiktok ") {
			cookies := strings.TrimSpace(body[len("!tiktok "):])
			if cookies == "" {
				sendText(cli, chatBare, "Uso: !tiktok <cookies>")
				return
			}
			if err := os.WriteFile(tiktokCookies, []byte(cookies), 0600); err != nil {
				sendText(cli, chatBare, "❌ Falha ao salvar cookies: "+err.Error())
			} else {
				sendText(cli, chatBare, "✅ Cookies do TikTok atualizados.")
			}
			return
		}
	}
}

func sendText(cli *whatsmeow.Client, to, text string) {
	jid, err := types.ParseJID(to)
	if err != nil {
		log.Printf("⚠️ JID inválido %q: %v", to, err)
		return
	}
	msg := &waProto.Message{Conversation: proto.String(text)}
	if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
		log.Printf("❌ falha ao enviar mensagem: %v", err)
	}
}

func main() {}
