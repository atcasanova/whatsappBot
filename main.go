package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"sync"
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

type AppState struct {
	openaiClient  *go_openai.Client
	model         string
	promptSummary string
	promptChatGPT string
	pathMp3       string
	userJID       string
	instaCookies  string
	tiktokCookies string
	downloadProxy string
	sessionPath   string

	client *whatsmeow.Client

	allowedGroups  map[string]bool
	messageHistory map[string][]Msg
	contactNames   map[string]string
	currentDay     int

	mu sync.RWMutex
}

func NewAppState() (*AppState, error) {
	userPhone := normalizePhone(mustEnv("USER_PHONE", ""))
	if userPhone == "" {
		return nil, fmt.Errorf("USER_PHONE não definido")
	}

	state := &AppState{
		openaiClient:   go_openai.NewClient(os.Getenv("OPENAI_API_KEY")),
		model:          mustEnv("MODEL", "gpt-4o-mini"),
		promptSummary:  mustEnv("PROMPT", "Faça um resumo das seguintes mensagens..."),
		promptChatGPT:  mustEnv("CHATGPT_PROMPT", "Responda ao questionamento a seguir..."),
		pathMp3:        mustEnv("PATH_MP3", "."),
		instaCookies:   mustEnv("INSTA_COOKIES_PATH", "./insta_cookies.txt"),
		tiktokCookies:  mustEnv("TIKTOK_COOKIES_PATH", "./tiktok_cookies.txt"),
		downloadProxy:  mustEnv("DOWNLOAD_PROXY", ""),
		sessionPath:    mustEnv("PATH_SESSION", "./"),
		userJID:        userPhone + "@s.whatsapp.net",
		allowedGroups:  make(map[string]bool),
		messageHistory: make(map[string][]Msg),
		contactNames:   make(map[string]string),
		currentDay:     time.Now().Day(),
	}

	for _, g := range strings.Split(mustEnv("GROUPS", ""), ",") {
		if g != "" {
			state.allowedGroups[g] = true
		}
	}

	return state, nil
}

func (s *AppState) ConnectClient() error {
	dbLog := waLog.Stdout("DB", "ERROR", true)
	dsn := fmt.Sprintf("file:%s/datastore.db?_foreign_keys=on", s.sessionPath)
	ctx := context.Background()
	sqlContainer, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		return fmt.Errorf("erro ao abrir store: %w", err)
	}
	deviceStore, err := sqlContainer.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("erro ao obter device store: %w", err)
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	s.client = client
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		go func() {
			for evt := range qrChan {
				fmt.Println("QR Code:", evt.Code)
			}
		}()
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("falha ao conectar: %w", err)
	}
	return nil
}

type Bot struct {
	cli            *whatsmeow.Client
	allowedGroups  map[string]bool
	messageHistory map[string][]Msg
	contactNames   map[string]string
	currentDay     int
	commands       map[string]func(*events.Message, messageContext) bool
}

type messageContext struct {
	senderFull   string
	senderBare   string
	chatBare     string
	infoIsFromMe bool
	body         string
	trimmedBody  string
	fromName     string
}

// helpers de contexto
type void struct{}

func isFromMe(state *AppState, sender string, infoIsFromMe bool) bool {
	if infoIsFromMe {
		return true
	}
	if sender == state.userJID {
		return true
	}
	if bareJID(sender) == state.userJID {
		return true
	}
	return false
}

func isPrivateChat(state *AppState, chat string) bool {
	return bareJID(chat) == state.userJID
}

func isAuthorizedGroup(state *AppState, chat string) bool {
	return state.isAllowedGroup(chat)
}

func logTriggerEvaluation(state *AppState, triggerName, chatBare, senderBare, senderFull, body string, infoIsFromMe bool) {
	trimmedBody := strings.TrimSpace(body)
	calculatedIsFromMe := isFromMe(state, senderBare, infoIsFromMe)
	log.Printf(
		"🔎 Trigger check %s: chat=%s authorized=%t allowedEntry=%t senderBare=%s senderFull=%s isFromMe=%t infoIsFromMe=%t userJID=%s bodyRaw=%q bodyTrimmed=%q matchesExact=%t historyCount=%d",
		triggerName,
		chatBare,
		isAuthorizedGroup(state, chatBare),
		state.isAllowedGroup(chatBare),
		senderBare,
		senderFull,
		calculatedIsFromMe,
		infoIsFromMe,
		state.userJID,
		body,
		trimmedBody,
		body == triggerName,
		state.historyCount(chatBare),
	)
}

func (s *AppState) isAllowedGroup(chat string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowedGroups[chat]
}

func (s *AppState) addAllowedGroup(chat string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowedGroups[chat] = true
}

func (s *AppState) removeAllowedGroup(chat string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.allowedGroups, chat)
}

func (s *AppState) allowedGroupList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]string, 0, len(s.allowedGroups))
	for g := range s.allowedGroups {
		list = append(list, g)
	}
	return list
}

func (s *AppState) historyCount(chat string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messageHistory[chat])
}

func (s *AppState) appendHistory(chat string, msg Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageHistory[chat] = append(s.messageHistory[chat], msg)
}

func (s *AppState) historyForChat(chat string) []Msg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Msg(nil), s.messageHistory[chat]...)
}

func (s *AppState) resetHistoryIfNeeded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Day() != s.currentDay {
		s.messageHistory = make(map[string][]Msg)
		s.currentDay = time.Now().Day()
	}
}

func (s *AppState) setContactName(jid, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contactNames[jid] = name
}

func (s *AppState) contactName(jid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name, ok := s.contactNames[jid]
	return name, ok
}

func bareJID(full string) string {
	parts := strings.SplitN(full, "@", 2)
	local := strings.SplitN(parts[0], ":", 2)[0]
	return local + "@" + parts[1]
}

func sendKeepAlive(cli *whatsmeow.Client) error {
	return cli.SendPresence(types.PresenceAvailable)
}

func jitteredInterval(r *rand.Rand, min, max time.Duration, jitterFraction float64) time.Duration {
	if max <= min {
		return min
	}
	span := max - min
	base := time.Duration(r.Int63n(int64(span))) + min

	jitterRange := time.Duration(float64(base) * jitterFraction)
	if jitterRange == 0 {
		return base
	}
	offset := time.Duration(r.Int63n(int64(jitterRange*2)+1)) - jitterRange
	return base + offset
}

func extractBody(v *events.Message) string {
	body := v.Message.GetConversation()
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		body = ext.GetText()
	}
	return body
}

func extractQuotedText(v *events.Message) string {
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		if ctx := ext.GetContextInfo(); ctx != nil {
			if qm := ctx.GetQuotedMessage(); qm != nil {
				quotedText := qm.GetConversation()
				if quotedText == "" && qm.GetExtendedTextMessage() != nil {
					quotedText = qm.GetExtendedTextMessage().GetText()
				}
				return quotedText
			}
		}
	}
	return ""
}

func extractQuotedImage(cli *whatsmeow.Client, v *events.Message) ([]byte, string) {
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		if ctx := ext.GetContextInfo(); ctx != nil {
			if qm := ctx.GetQuotedMessage(); qm != nil {
				if img := qm.GetImageMessage(); img != nil {
					data, err := cli.Download(context.Background(), img)
					if err != nil {
						log.Printf("⚠️ Falha ao baixar imagem citada: %v", err)
						return nil, ""
					}
					mimeType := img.GetMimetype()
					if mimeType == "" {
						mimeType = http.DetectContentType(data)
					}
					return data, mimeType
				}
			}
		}
	}
	return nil, ""
}

func extractQuotedAudio(v *events.Message) (*waProto.AudioMessage, string) {
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		if ctx := ext.GetContextInfo(); ctx != nil {
			if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
				return qm.GetAudioMessage(), ctx.GetStanzaID()
			}
		}
	}
	return nil, ""
}

func commandName(trimmedBody string) string {
	if trimmedBody == "" {
		return ""
	}
	cmd := trimmedBody
	if idx := strings.IndexAny(trimmedBody, " \t\n"); idx != -1 {
		cmd = trimmedBody[:idx]
	}
	return cmd
}

func NewBot(cli *whatsmeow.Client, allowedGroups map[string]bool) *Bot {
	bot := &Bot{
		cli:            cli,
		allowedGroups:  allowedGroups,
		messageHistory: make(map[string][]Msg),
		contactNames:   make(map[string]string),
		currentDay:     time.Now().Day(),
	}
	bot.commands = map[string]func(*events.Message, messageContext) bool{
		"!carteirinha": bot.handleCarteirinha,
		"!cnh":         bot.handleCNH,
		"!chatgpt":     bot.handleChatGPT,
		"!img":         bot.handleImg,
		"!download":    bot.handleDownload,
		"!ler":         bot.handleLer,
		"!podcast":     bot.handlePodcast,
		"!resumo":      bot.handleResumo,
		"!logs":        bot.handleLogs,
		"!grupos":      bot.handleGrupos,
		"!model":       bot.handleModel,
		"!insta":       bot.handleInstaCookies,
		"!tiktok":      bot.handleTiktokCookies,
	}
	return bot
}

func (b *Bot) isAuthorizedGroup(chat string) bool {
	return b.allowedGroups[chat]
}

func (b *Bot) logTriggerEvaluation(triggerName, chatBare, senderBare, senderFull, body string, infoIsFromMe bool) {
	trimmedBody := strings.TrimSpace(body)
	calculatedIsFromMe := isFromMe(senderBare, infoIsFromMe)
	log.Printf(
		"🔎 Trigger check %s: chat=%s authorized=%t allowedEntry=%t senderBare=%s senderFull=%s isFromMe=%t infoIsFromMe=%t userJID=%s bodyRaw=%q bodyTrimmed=%q matchesExact=%t historyCount=%d",
		triggerName,
		chatBare,
		b.isAuthorizedGroup(chatBare),
		b.allowedGroups[chatBare],
		senderBare,
		senderFull,
		calculatedIsFromMe,
		infoIsFromMe,
		userJID,
		body,
		trimmedBody,
		body == triggerName,
		len(b.messageHistory[chatBare]),
	)
}

func (b *Bot) triggerKeepAlive(trimmedBody string) {
	if !strings.HasPrefix(trimmedBody, "!") {
		return
	}
	go func() {
		if err := sendKeepAlive(b.cli); err != nil {
			log.Printf("⚠️ keep-alive falhou ao detectar trigger: %v", err)
		}
	}()
}

func (b *Bot) buildContext(v *events.Message) messageContext {
	body := extractBody(v)
	ctx := messageContext{
		senderFull:   v.Info.Sender.String(),
		senderBare:   bareJID(v.Info.Sender.String()),
		chatBare:     bareJID(v.Info.Chat.String()),
		infoIsFromMe: v.Info.IsFromMe,
		body:         body,
		trimmedBody:  strings.TrimSpace(body),
		fromName:     v.Info.PushName,
	}
	if ctx.fromName == "" {
		ctx.fromName = ctx.senderBare
	}
	return ctx
}

func startKeepAliveLoop(cli *whatsmeow.Client) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	const minInterval = 6 * time.Hour
	const maxInterval = 48 * time.Hour
	const jitterFraction = 0.10
	backoff := minInterval

	go func() {
		for {
			interval := jitteredInterval(r, minInterval, maxInterval, jitterFraction)
			if backoff > interval {
				interval = backoff
			}
			time.Sleep(interval)

			if err := sendKeepAlive(cli); err != nil {
				log.Printf("⚠️ keep-alive falhou: %v", err)
				backoff = time.Duration(math.Min(float64(backoff*2), float64(maxInterval)))
				continue
			}

			backoff = minInterval
		}
	}()
}

func mustEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func normalizePhone(phone string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
}

var reVideoURL = regexp.MustCompile(`https?://[^\s]*?(instagram\.com|threads\.net|tiktok\.com|vm\.tiktok\.com|vt\.tiktok\.com|youtube\.com|youtu\.be|x\.com|twitter\.com)[^\s]*`)

func convertVideoToMP4(inputPath string) (string, error) {
	outputPath := strings.TrimSuffix(inputPath, path.Ext(inputPath)) + "_converted.mp4"
	args := []string{
		"-y",
		"-i", inputPath,
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-movflags", "+faststart",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		outputPath,
	}
	log.Printf("🎬 ffmpeg %v", args)
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("ffmpeg output: %s", string(out))
	}
	if err != nil {
		return "", fmt.Errorf("ffmpeg falhou: %w", err)
	}
	return outputPath, nil
}

func extractVideoURL(text string) string {
	match := reVideoURL.FindString(text)
	return match
}

func cookieArgsForURL(state *AppState, url string) []string {
	cookiePath := ""
	switch {
	case strings.Contains(url, "instagram.com") || strings.Contains(url, "threads.net"):
		cookiePath = state.instaCookies
	case strings.Contains(url, "tiktok.com"):
		cookiePath = state.tiktokCookies
	default:
		return nil
	}

	if cookiePath == "" {
		return nil
	}

	if _, err := os.Stat(cookiePath); err == nil {
		return []string{"--cookies", cookiePath}
	} else {
		log.Printf("⚠️ cookies file not found %s: %v", cookiePath, err)
		return nil
	}
}

func runYtDlp(args []string) error {
	log.Printf("▶️ yt-dlp %v", args)
	cmd := exec.Command("yt-dlp", args...)
	out, err := cmd.CombinedOutput()
	log.Printf("yt-dlp output: %s", string(out))
	if err != nil {
		log.Printf("yt-dlp erro: %v", err)
		return err
	}
	return nil
}

func downloadAndSendMedia(state *AppState, cli *whatsmeow.Client, chat string, url string) {
	tmpDir, err := os.MkdirTemp("", "vid-*")
	if err != nil {
		log.Printf("erro temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	tmpl := path.Join(tmpDir, "%(id)s.%(ext)s")
	cookieArgs := cookieArgsForURL(state, url)

	type downloadAttempt struct {
		name           string
		includeProxy   bool
		includeCookies bool
	}

	attempts := []downloadAttempt{
		{name: "proxy sem cookies", includeProxy: true, includeCookies: false},
		{name: "proxy com cookies", includeProxy: true, includeCookies: true},
		{name: "direto com cookies", includeProxy: false, includeCookies: true},
	}

	var files []os.DirEntry
	for _, attempt := range attempts {
		args := []string{}
		if attempt.includeProxy {
			if state.downloadProxy == "" {
				log.Printf("⚠️ proxy não configurado, pulando tentativa %s", attempt.name)
				continue
			}
			args = append(args, "--proxy", state.downloadProxy)
		}
		if attempt.includeCookies {
			if len(cookieArgs) == 0 {
				log.Printf("⚠️ cookies não disponíveis, pulando tentativa %s", attempt.name)
				continue
			}
			args = append(args, cookieArgs...)
		}
		args = append(args, "-o", tmpl, url)

		if err := runYtDlp(args); err != nil {
			continue
		}

		files, err = os.ReadDir(tmpDir)
		if err != nil {
			log.Printf("erro listando diretório temporário: %v", err)
			return
		}
		if len(files) > 0 {
			break
		}
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
			convertedPath, err := convertVideoToMP4(fp)
			if err != nil {
				log.Printf("⚠️ falha ao converter video: %v", err)
			} else {
				fp = convertedPath
				data, err = os.ReadFile(fp)
				if err != nil {
					log.Printf("erro lendo video convertido %s: %v", fp, err)
					continue
				}
				mimeType = "video/mp4"
			}
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

func sendImageFromFile(cli *whatsmeow.Client, chat, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("falha ao ler %s: %w", filePath, err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(filePath)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return fmt.Errorf("arquivo %s não é imagem (mime=%s)", filePath, mimeType)
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return fmt.Errorf("JID inválido %q: %w", chat, err)
	}
	up, err := cli.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload da imagem falhou: %w", err)
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
		return fmt.Errorf("falha ao enviar imagem: %w", err)
	}
	return nil
}

func sendDocumentFromFile(cli *whatsmeow.Client, chat, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("falha ao ler %s: %w", filePath, err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(filePath)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return fmt.Errorf("JID inválido %q: %w", chat, err)
	}
	up, err := cli.Upload(context.Background(), data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("upload do documento falhou: %w", err)
	}
	docMsg := &waProto.DocumentMessage{
		Mimetype:      proto.String(mimeType),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
		FileName:      proto.String(path.Base(filePath)),
	}
	if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{DocumentMessage: docMsg}); err != nil {
		return fmt.Errorf("falha ao enviar documento: %w", err)
	}
	return nil
}

type config struct {
	sessionPath string
}

func loadConfig() (*config, error) {
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
	userPhone := normalizePhone(mustEnv("USER_PHONE", ""))
	if userPhone == "" {
		return nil, fmt.Errorf("USER_PHONE não definido")
	}
	userJID = userPhone + "@s.whatsapp.net"

	model = mustEnv("MODEL", "gpt-4o-mini")
	promptSummary = mustEnv("PROMPT", "Faça um resumo das seguintes mensagens...")
	promptChatGPT = mustEnv("CHATGPT_PROMPT", "Responda ao questionamento a seguir...")
	instaCookies = mustEnv("INSTA_COOKIES_PATH", "./insta_cookies.txt")
	tiktokCookies = mustEnv("TIKTOK_COOKIES_PATH", "./tiktok_cookies.txt")
	downloadProxy = mustEnv("DOWNLOAD_PROXY", "")

	allowedGroups := make(map[string]bool)
	for _, g := range strings.Split(mustEnv("GROUPS", ""), ",") {
		if g != "" {
			allowedGroups[g] = true
		}
	}

	return &config{sessionPath: sessionPath}, nil
}

func newClient(ctx context.Context, cfg *config) (*whatsmeow.Client, error) {
	dbLog := waLog.Stdout("DB", "ERROR", true)
	dsn := fmt.Sprintf("file:%s/datastore.db?_foreign_keys=on", cfg.sessionPath)
	sqlContainer, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		return nil, fmt.Errorf("erro ao abrir store: %w", err)
	}
	deviceStore, err := sqlContainer.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("erro ao obter device store: %w", err)
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	return client, nil
}

func runBot(ctx context.Context, client *whatsmeow.Client, cfg *config) error {
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(ctx)
		go func() {
			for evt := range qrChan {
				fmt.Println("QR Code:", evt.Code)
			}
		}()
	}

	if err := client.Connect(); err != nil {
		return fmt.Errorf("falha ao conectar: %w", err)
	}
	startKeepAliveLoop(client)

	errCh := make(chan error, 1)
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.LoggedOut:
			log.Println("⚠️ logout remoto, limpando sessão e reiniciando...")
			os.RemoveAll(cfg.sessionPath)
			select {
			case errCh <- fmt.Errorf("logout remoto"):
			default:
			}
		case *events.Message:
			handleMessage(state, state.client, v)
		}
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("📴 sinal %s recebido, desconectando...", sig)
		client.Disconnect()
		return nil
	case err := <-errCh:
		client.Disconnect()
		return err
	}
}

func handleMessage(state *AppState, cli *whatsmeow.Client, v *events.Message) {
	// JIDs
	senderFull := v.Info.Sender.String()
	senderBare := bareJID(senderFull)
	chatBare := bareJID(v.Info.Chat.String())
	senderJID := senderBare
	infoIsFromMe := v.Info.IsFromMe

	if ctx.chatBare == "status@broadcast" || strings.HasSuffix(ctx.chatBare, "@newsletter") {
		return
	}

	// atualiza cache de nomes
	fromName := v.Info.PushName
	if fromName == "" {
		fromName = senderBare
	}
	state.setContactName(senderBare, fromName)

	if reaction := v.Message.GetReactionMessage(); reaction != nil {
		reactionText := reaction.GetText()
		targetID := ""
		targetChat := ctx.chatBare
		if key := reaction.GetKey(); key != nil {
			targetID = key.GetID()
			if remote := key.GetRemoteJID(); remote != "" {
				targetChat = bareJID(remote)
			}
		}
		log.Printf("😊 Reaction=%q from=%s chat=%s msgID=%s", reactionText, ctx.senderBare, targetChat, targetID)
		return
	}

	log.Printf("📥 DEBUG sender=%s chat=%s body=%q", ctx.senderBare, ctx.chatBare, ctx.body)

	if strings.HasPrefix(ctx.trimmedBody, "!") {
		b.logTriggerEvaluation(commandName(ctx.trimmedBody), ctx.chatBare, ctx.senderBare, ctx.senderFull, ctx.body, ctx.infoIsFromMe)
	}
	b.triggerKeepAlive(ctx.trimmedBody)

	b.resetDailyIfNeeded()
	b.saveAudioIfPresent(v)

	if handler, ok := b.commands[commandName(ctx.trimmedBody)]; ok {
		if handler(v, ctx) {
			return
		}
		logTriggerEvaluation(state, commandName, chatBare, senderBare, senderFull, body, infoIsFromMe)
		go func() {
			if err := sendKeepAlive(cli); err != nil {
				log.Printf("⚠️ keep-alive falhou ao detectar trigger: %v", err)
			}
		}()
	}

	// reset diário
	state.resetHistoryIfNeeded()

func (b *Bot) saveAudioIfPresent(v *events.Message) {
	if aud := v.Message.GetAudioMessage(); aud != nil {
		data, err := b.cli.Download(context.Background(), aud)
		if err == nil {
			exts, _ := mime.ExtensionsByType(aud.GetMimetype())
			var ext string
			if len(exts) > 0 {
				ext = exts[0]
			} else {
				ext = ".ogg"
			}
			fn := path.Join(state.pathMp3, v.Info.ID+ext)
			_ = os.WriteFile(fn, data, 0644)
			log.Println("✅ Baixou Audio")
		}
	}
}

	// ==== comandos GLOBAIS (qualquer chat) ====
	if isFromMe(state, senderJID, infoIsFromMe) {
		if trimmedBody == "!carteirinha" {
			log.Println("✅ Disparou !carteirinha")
			if err := sendImageFromFile(cli, chatBare, "carteirinha.jpg"); err != nil {
				log.Printf("❌ Falha ao enviar carteirinha: %v", err)
				sendText(cli, chatBare, "❌ "+err.Error())
			}
			return
		}
		if trimmedBody == "!cnh" {
			log.Println("✅ Disparou !cnh")
			if err := sendDocumentFromFile(cli, chatBare, "cnh.pdf"); err != nil {
				log.Printf("❌ Falha ao enviar CNH: %v", err)
				sendText(cli, chatBare, "❌ "+err.Error())
			}
			return quotedBody, quotedFrom
		}
		// !chatgpt
		if strings.HasPrefix(body, "!chatgpt") {
			log.Println("✅ Disparou !chatgpt")
			ext := v.Message.GetExtendedTextMessage()
			userMsg := strings.TrimSpace(body[len("!chatgpt"):])
			var (
				quotedText  string
				quotedImage []byte
				quotedMime  string
			)
			if ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil {
						quotedText = qm.GetConversation()
						if quotedText == "" && qm.GetExtendedTextMessage() != nil {
							quotedText = qm.GetExtendedTextMessage().GetText()
						}
						if img := qm.GetImageMessage(); img != nil {
							if data, err := cli.Download(context.Background(), img); err != nil {
								log.Printf("⚠️ Falha ao baixar imagem citada: %v", err)
							} else {
								quotedImage = data
								quotedMime = img.GetMimetype()
								if quotedMime == "" {
									quotedMime = http.DetectContentType(quotedImage)
								}
							}
						}
					}
				}
			}
			prompt := userMsg
			if quotedText != "" {
				prompt = fmt.Sprintf("%s\n\nMensagem citada: %s", userMsg, quotedText)
			}
			if prompt != "" || len(quotedImage) > 0 {
				message := go_openai.ChatCompletionMessage{Role: go_openai.ChatMessageRoleUser}
				if len(quotedImage) > 0 {
					if quotedMime == "" {
						quotedMime = "image/jpeg"
					}
					message.MultiContent = []go_openai.ChatMessagePart{
						{
							Type: go_openai.ChatMessagePartTypeText,
							Text: state.promptChatGPT + "\n\n" + prompt,
						},
						{
							Type: go_openai.ChatMessagePartTypeImageURL,
							ImageURL: &go_openai.ChatMessageImageURL{
								URL: fmt.Sprintf("data:%s;base64,%s", quotedMime, base64.StdEncoding.EncodeToString(quotedImage)),
							},
						},
					}
				} else {
					message.Content = state.promptChatGPT + "\n\n" + prompt
				}
				req := go_openai.ChatCompletionRequest{
					Model:    state.model,
					Messages: []go_openai.ChatCompletionMessage{message},
				}
				if resp, err := state.openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
					sendText(cli, chatBare, resp.Choices[0].Message.Content)
				}
			}
			return

func (b *Bot) handleCNH(_ *events.Message, ctx messageContext) bool {
	if !isFromMe(ctx.senderBare, ctx.infoIsFromMe) {
		return false
	}
	log.Println("✅ Disparou !cnh")
	if err := sendDocumentFromFile(b.cli, ctx.chatBare, "cnh.pdf"); err != nil {
		log.Printf("❌ Falha ao enviar CNH: %v", err)
		sendText(b.cli, ctx.chatBare, "❌ "+err.Error())
	}
	return true
}

func (b *Bot) handleChatGPT(v *events.Message, ctx messageContext) bool {
	if !isFromMe(ctx.senderBare, ctx.infoIsFromMe) {
		return false
	}
	log.Println("✅ Disparou !chatgpt")
	userMsg := strings.TrimSpace(ctx.body[len("!chatgpt"):])
	quotedText := extractQuotedText(v)
	quotedImage, quotedMime := extractQuotedImage(b.cli, v)
	prompt := userMsg
	if quotedText != "" {
		prompt = fmt.Sprintf("%s\n\nMensagem citada: %s", userMsg, quotedText)
	}
	if prompt == "" && len(quotedImage) == 0 {
		return true
	}
	message := go_openai.ChatCompletionMessage{Role: go_openai.ChatMessageRoleUser}
	if len(quotedImage) > 0 {
		if quotedMime == "" {
			quotedMime = "image/jpeg"
		}
		// !img
		if strings.HasPrefix(body, "!img ") {
			prompt := strings.TrimSpace(body[len("!img "):])
			log.Printf("🖼️ Gerando imagem para: %q", prompt)
			respImg, err := state.openaiClient.CreateImage(
				context.Background(),
				go_openai.ImageRequest{
					Prompt:  prompt,
					N:       1,
					Size:    go_openai.CreateImageSize1024x1024,
					Model:   "gpt-image-1",
					Quality: "high",
				},
			},
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
				go downloadAndSendMedia(state, cli, chatBare, url)
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
						orig := ctx.GetStanzaID()
						filePath := path.Join(state.pathMp3, orig+ext)
						tr, err := state.openaiClient.CreateTranscription(
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
		// !podcast
		if body == "!podcast" {
			log.Println("✅ Disparou !podcast")
			if ext := v.Message.GetExtendedTextMessage(); ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
						aud := qm.GetAudioMessage()
						exts, _ := mime.ExtensionsByType(aud.GetMimetype())
						ext := ".ogg"
						if len(exts) > 0 {
							ext = exts[0]
						}
						orig := ctx.GetStanzaID()
						filePath := path.Join(state.pathMp3, orig+ext)
						tr, err := state.openaiClient.CreateTranscription(
							context.Background(),
							go_openai.AudioRequest{Model: go_openai.Whisper1, FilePath: filePath},
						)
						if err != nil {
							sendText(cli, chatBare, "❌ Erro na transcrição: "+err.Error())
						} else {
							req := go_openai.ChatCompletionRequest{
								Model: state.model,
								Messages: []go_openai.ChatCompletionMessage{
									{
										Role:    go_openai.ChatMessageRoleSystem,
										Content: "Você é um assistente que interpreta áudios transcritos e explica com clareza a mensagem principal.",
									},
									{
										Role:    go_openai.ChatMessageRoleUser,
										Content: fmt.Sprintf("Transcrição literal do áudio:\n\n%s\n\nExplique de forma muito clara o que a pessoa quis dizer.", tr.Text),
									},
								},
								Temperature: 1,
							}
							if resp, err := state.openaiClient.CreateChatCompletion(context.Background(), req); err != nil {
								sendText(cli, chatBare, "❌ Erro ao resumir: "+err.Error())
							} else {
								sendText(cli, chatBare, "🎧 "+strings.TrimSpace(resp.Choices[0].Message.Content))
							}
						}
					}
				}
			}
			return
		}
		mimeType = "image/png"
	} else {
		sendText(b.cli, ctx.chatBare, "❌ Resposta da API sem imagem")
		return true
	}
	up, err := b.cli.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
	if err != nil {
		sendText(b.cli, ctx.chatBare, "❌ Erro no upload da imagem: "+err.Error())
		return true
	}
	jid, err := types.ParseJID(ctx.chatBare)
	if err != nil {
		log.Printf("⚠️ JID inválido: %v", err)
		return true
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
	if _, err := b.cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imageMsg}); err != nil {
		log.Printf("❌ falha ao enviar imagem: %v", err)
	}
	return true
}

	// ==== comando !resumo (antes de gravar) ====
	if isAuthorizedGroup(state, chatBare) && isFromMe(state, senderJID, infoIsFromMe) && body == "!resumo" {
		log.Println("✅ Disparou !resumo")
		logs := state.historyForChat(chatBare)
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
			Model: state.model,
			Messages: []go_openai.ChatCompletionMessage{{
				Role:    go_openai.ChatMessageRoleUser,
				Content: state.promptSummary + "\n\n" + sb.String(),
			}},
		}
		if resp, err := state.openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
			sendText(cli, chatBare, summaryMarker+" Resumo:\n"+resp.Choices[0].Message.Content)
		}
		return
	}
	if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err != nil {
		sendText(b.cli, ctx.chatBare, "❌ Erro ao resumir: "+err.Error())
	} else {
		sendText(b.cli, ctx.chatBare, "🎧 "+strings.TrimSpace(resp.Choices[0].Message.Content))
	}
	return true
}

	// ==== grava histórico (ignora comandos, resumo e bodies vazios) ====
	if isAuthorizedGroup(state, chatBare) &&
		strings.TrimSpace(body) != "" &&
		!strings.HasPrefix(body, "!resumo") &&
		!strings.HasPrefix(body, summaryMarker) {

		var qf, qb string
		if ext := v.Message.GetExtendedTextMessage(); ext != nil {
			if ctx := ext.GetContextInfo(); ctx != nil && ctx.GetQuotedMessage() != nil {
				qb = ctx.GetQuotedMessage().GetConversation()
				quoted := bareJID(ctx.GetParticipant())
				if name, ok := state.contactName(quoted); ok {
					qf = fmt.Sprintf("%s (%s)", name, quoted)
				} else {
					qf = quoted
				}
			}
		}
		state.appendHistory(chatBare, Msg{
			From:       fromName,
			Body:       body,
			Timestamp:  v.Info.Timestamp,
			QuotedFrom: qf,
			QuotedBody: qb,
		})
	}
	req := go_openai.ChatCompletionRequest{
		Model: model,
		Messages: []go_openai.ChatCompletionMessage{{
			Role:    go_openai.ChatMessageRoleUser,
			Content: promptSummary + "\n\n" + sb.String(),
		}},
	}
	if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
		sendText(b.cli, ctx.chatBare, summaryMarker+" Resumo:\n"+resp.Choices[0].Message.Content)
	}
	return true
}

	// ==== comandos na MINHA DM (!logs, !model, !grupos) ====
	if isFromMe(state, senderJID, infoIsFromMe) && isPrivateChat(state, chatBare) {
		if strings.HasPrefix(body, "!logs ") {
			parts := strings.Fields(body)
			if len(parts) != 2 {
				sendText(cli, chatBare, "Uso: !logs <groupJID>")
			} else {
				gid := parts[1]
				logs := state.historyForChat(gid)
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
				list := state.allowedGroupList()
				if len(list) == 0 {
					sendText(cli, chatBare, "Nenhum grupo autorizado.")
				} else {
					sendText(cli, chatBare, "Grupos autorizados: "+strings.Join(list, ", "))
				}
			case len(parts) == 3 && parts[1] == "add":
				gid := parts[2]
				state.addAllowedGroup(gid)
				sendText(cli, chatBare, fmt.Sprintf("✅ Grupo %s adicionado.", gid))
			case len(parts) == 3 && parts[1] == "del":
				gid := parts[2]
				state.removeAllowedGroup(gid)
				sendText(cli, chatBare, fmt.Sprintf("✅ Grupo %s removido.", gid))
			default:
				sendText(cli, chatBare, "Uso: !grupos [add|del] <chatJID>")
			}
			return
		}
		if body == "!model" {
			sendText(cli, chatBare, fmt.Sprintf("Modelo atual: %s", state.model))
			return
		}
		if strings.HasPrefix(body, "!model ") {
			newModel := strings.TrimSpace(body[len("!model "):])
			state.model = newModel
			sendText(cli, chatBare, fmt.Sprintf("✅ Modelo alterado para %s", state.model))
			return
		}
		if strings.HasPrefix(body, "!insta ") {
			cookies := strings.TrimSpace(body[len("!insta "):])
			if cookies == "" {
				sendText(cli, chatBare, "Uso: !insta <cookies>")
				return
			}
			if err := os.WriteFile(state.instaCookies, []byte(cookies), 0600); err != nil {
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
			if err := os.WriteFile(state.tiktokCookies, []byte(cookies), 0600); err != nil {
				sendText(cli, chatBare, "❌ Falha ao salvar cookies: "+err.Error())
			} else {
				sendText(cli, chatBare, "✅ Cookies do TikTok atualizados.")
			}
			return
		}
	case len(parts) == 3 && parts[1] == "add":
		gid := parts[2]
		b.allowedGroups[gid] = true
		sendText(b.cli, ctx.chatBare, fmt.Sprintf("✅ Grupo %s adicionado.", gid))
	case len(parts) == 3 && parts[1] == "del":
		gid := parts[2]
		delete(b.allowedGroups, gid)
		sendText(b.cli, ctx.chatBare, fmt.Sprintf("✅ Grupo %s removido.", gid))
	default:
		sendText(b.cli, ctx.chatBare, "Uso: !grupos [add|del] <chatJID>")
	}
	return true
}

func (b *Bot) handleModel(_ *events.Message, ctx messageContext) bool {
	if !isFromMe(ctx.senderBare, ctx.infoIsFromMe) || !isPrivateChat(ctx.chatBare) || !strings.HasPrefix(ctx.trimmedBody, "!model") {
		return false
	}
	if ctx.trimmedBody == "!model" {
		sendText(b.cli, ctx.chatBare, fmt.Sprintf("Modelo atual: %s", model))
		return true
	}
	newModel := strings.TrimSpace(ctx.trimmedBody[len("!model "):])
	model = newModel
	sendText(b.cli, ctx.chatBare, fmt.Sprintf("✅ Modelo alterado para %s", model))
	return true
}

func (b *Bot) handleInstaCookies(_ *events.Message, ctx messageContext) bool {
	if !isFromMe(ctx.senderBare, ctx.infoIsFromMe) || !isPrivateChat(ctx.chatBare) || !strings.HasPrefix(ctx.trimmedBody, "!insta ") {
		return false
	}
	cookies := strings.TrimSpace(ctx.trimmedBody[len("!insta "):])
	if cookies == "" {
		sendText(b.cli, ctx.chatBare, "Uso: !insta <cookies>")
		return true
	}
	if err := os.WriteFile(instaCookies, []byte(cookies), 0600); err != nil {
		sendText(b.cli, ctx.chatBare, "❌ Falha ao salvar cookies: "+err.Error())
	} else {
		sendText(b.cli, ctx.chatBare, "✅ Cookies do Instagram atualizados.")
	}
	return true
}

func (b *Bot) handleTiktokCookies(_ *events.Message, ctx messageContext) bool {
	if !isFromMe(ctx.senderBare, ctx.infoIsFromMe) || !isPrivateChat(ctx.chatBare) || !strings.HasPrefix(ctx.trimmedBody, "!tiktok ") {
		return false
	}
	cookies := strings.TrimSpace(ctx.trimmedBody[len("!tiktok "):])
	if cookies == "" {
		sendText(b.cli, ctx.chatBare, "Uso: !tiktok <cookies>")
		return true
	}
	if err := os.WriteFile(tiktokCookies, []byte(cookies), 0600); err != nil {
		sendText(b.cli, ctx.chatBare, "❌ Falha ao salvar cookies: "+err.Error())
	} else {
		sendText(b.cli, ctx.chatBare, "✅ Cookies do TikTok atualizados.")
	}
	return true
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

func main() {
	ctx := context.Background()

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("erro carregando configuração: %v", err)
		os.Exit(1)
	}

	client, err := newClient(ctx, cfg)
	if err != nil {
		log.Printf("erro criando cliente: %v", err)
		os.Exit(1)
	}

	if err := runBot(ctx, client, cfg); err != nil {
		log.Printf("erro executando bot: %v", err)
		os.Exit(1)
	}
}
