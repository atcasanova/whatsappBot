package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	go_openai "github.com/sashabaranov/go-openai"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"go.mau.fi/whatsmeow"
	waCompanionReg "go.mau.fi/whatsmeow/proto/waCompanionReg"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	waWa6 "go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	summaryMarker       = "📋󠅢󠅕󠅣󠅥󠅝󠅟"
	maxStickerSizeBytes = 900 * 1024 // keep stickers safely under WhatsApp's 1MB limit
)

// Msg representa uma mensagem armazenada, possivelmente com quote
type Msg struct {
	From       string
	Body       string
	Timestamp  time.Time
	QuotedFrom string // nome + bare JID de quem foi citado
	QuotedBody string // texto citado
}

type savedTrigger struct {
	Name     string
	Kind     string
	MimeType string
	FileName string
	Data     []byte
	Text     string
	Caption  string
	IsPTT    bool
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
	downloadProxy  string
	messageHistory = make(map[string][]Msg)
	currentDay     = time.Now().Day()
	contactNames   = make(map[string]string)
	triggerDB      *sql.DB
)

var reservedTriggers = map[string]struct{}{
	"salvar":      {},
	"triggers":    {},
	"carteirinha": {},
	"cnh":         {},
	"pix":         {},
	"copia":       {},
	"chatgpt":     {},
	"edit":        {},
	"img":         {},
	"sticker":     {},
	"download":    {},
	"print":       {},
	"paywall":     {},
	"ler":         {},
	"podcast":     {},
	"resumo":      {},
	"logs":        {},
	"grupos":      {},
	"model":       {},
	"insta":       {},
	"tiktok":      {},
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// helpers de contexto
type void struct{}

func isFromMe(sender string, infoIsFromMe bool) bool {
	if infoIsFromMe {
		return true
	}
	if sender == userJID {
		return true
	}
	if bareJID(sender) == userJID {
		return true
	}
	return false
}

func isPrivateChat(chat string) bool {
	return bareJID(chat) == userJID
}

func isAuthorizedGroup(chat string) bool {
	return allowedGroups[chat]
}

func logTriggerEvaluation(triggerName, chatBare, senderBare, senderFull, body string, infoIsFromMe bool) {
	trimmedBody := strings.TrimSpace(body)
	calculatedIsFromMe := isFromMe(senderBare, infoIsFromMe)
	log.Printf(
		"🔎 Trigger check %s: chat=%s authorized=%t allowedEntry=%t senderBare=%s senderFull=%s isFromMe=%t infoIsFromMe=%t userJID=%s bodyRaw=%q bodyTrimmed=%q matchesExact=%t historyCount=%d",
		triggerName,
		chatBare,
		isAuthorizedGroup(chatBare),
		allowedGroups[chatBare],
		senderBare,
		senderFull,
		calculatedIsFromMe,
		infoIsFromMe,
		userJID,
		body,
		trimmedBody,
		body == triggerName,
		len(messageHistory[chatBare]),
	)
}

func bareJID(full string) string {
	parts := strings.SplitN(full, "@", 2)
	local := strings.SplitN(parts[0], ":", 2)[0]
	return local + "@" + parts[1]
}

func sendKeepAlive(ctx context.Context, cli *whatsmeow.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := cli.SendPresence(ctx, types.PresenceAvailable); err != nil {
		return fmt.Errorf("presence available: %w", err)
	}

	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := cli.SendPresence(ctx, types.PresenceUnavailable); err != nil {
		return fmt.Errorf("presence unavailable: %w", err)
	}

	return nil
}

func createImageEdit(ctx context.Context, pngData []byte, prompt string) ([]byte, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	imageHeader := textproto.MIMEHeader{}
	imageHeader.Set("Content-Disposition", `form-data; name="image"; filename="image.png"`)
	imageHeader.Set("Content-Type", "image/png")

	imgPart, err := writer.CreatePart(imageHeader)
	if err != nil {
		return nil, fmt.Errorf("falha ao preparar parte da imagem: %w", err)
	}
	if _, err := imgPart.Write(pngData); err != nil {
		return nil, fmt.Errorf("falha ao escrever imagem: %w", err)
	}

	fields := map[string]string{
		"prompt": prompt,
		"model":  "gpt-image-1.5",
		"n":      "1",
	}
	if size := strings.TrimSpace(os.Getenv("OPENAI_IMAGE_SIZE")); size != "" {
		fields["size"] = size
	}
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return nil, fmt.Errorf("falha ao escrever campo %s: %w", k, err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("falha ao finalizar multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/images/edits", &body)
	if err != nil {
		return nil, fmt.Errorf("falha ao criar requisição: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("falha ao chamar API de edição: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("erro da API (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var imageResp go_openai.ImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&imageResp); err != nil {
		return nil, fmt.Errorf("falha ao decodificar resposta: %w", err)
	}
	if len(imageResp.Data) == 0 {
		return nil, fmt.Errorf("resposta sem imagem gerada")
	}

	data := imageResp.Data[0]
	if data.B64JSON != "" {
		editedBytes, err := base64.StdEncoding.DecodeString(data.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("falha ao decodificar imagem: %w", err)
		}
		return editedBytes, nil
	}

	if data.URL != "" {
		imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, data.URL, nil)
		if err != nil {
			return nil, fmt.Errorf("falha ao criar requisição de download: %w", err)
		}
		imgResp, err := http.DefaultClient.Do(imgReq)
		if err != nil {
			return nil, fmt.Errorf("falha ao baixar imagem: %w", err)
		}
		defer imgResp.Body.Close()
		if imgResp.StatusCode < 200 || imgResp.StatusCode >= 300 {
			bodyBytes, _ := io.ReadAll(imgResp.Body)
			return nil, fmt.Errorf("download da imagem retornou %d: %s", imgResp.StatusCode, string(bodyBytes))
		}
		return io.ReadAll(imgResp.Body)
	}

	return nil, fmt.Errorf("resposta sem dados de imagem")
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func triggerKeepAlive(cli *whatsmeow.Client) {
	go func() {
		if err := sendKeepAlive(context.Background(), cli); err != nil {
			log.Printf("⚠️ keep-alive falhou: %v", err)
		}
	}()
}

func configureClientIdentity() {
	store.SetOSInfo("Mac OS", [3]uint32{14, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()

	ua := store.BaseClientPayload.UserAgent
	ua.Platform = waWa6.ClientPayload_UserAgent_MACOS.Enum()
	ua.DeviceType = waWa6.ClientPayload_UserAgent_DESKTOP.Enum()
	ua.Manufacturer = proto.String("Apple")
	ua.Device = proto.String("Chrome on macOS")
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

func normalizePixAmount(raw string) string {
	trimmed := strings.TrimSpace(raw)
	normalized := strings.ReplaceAll(trimmed, ",", ".")
	var b strings.Builder
	dotSeen := false
	for _, r := range normalized {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' && !dotSeen:
			if b.Len() == 0 {
				b.WriteByte('0')
			}
			b.WriteRune(r)
			dotSeen = true
		}
	}
	return b.String()
}

func sanitizePixValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	return strings.Trim(trimmed, "\"'")
}

func pixField(id, value string) string {
	return fmt.Sprintf("%s%02d%s", id, len(value), value)
}

func crc16CCITT(input string) string {
	crc := 0xFFFF
	for i := 0; i < len(input); i++ {
		crc ^= int(input[i]) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
			crc &= 0xFFFF
		}
	}
	return fmt.Sprintf("%04X", crc)
}

func buildPixPayload(email, name, city, amount string) string {
	gui := "br.gov.bcb.pix"
	txid := "***"
	merchantAccount := pixField("00", gui) + pixField("01", email)
	payload := pixField("00", "01") +
		pixField("26", merchantAccount) +
		pixField("52", "0000") +
		pixField("53", "986")
	if amount != "" {
		payload += pixField("54", amount)
	}
	payload += pixField("58", "BR") +
		pixField("59", strings.ToUpper(name)) +
		pixField("60", strings.ToUpper(strings.ReplaceAll(city, " ", "."))) +
		pixField("62", pixField("05", txid)) +
		"6304"

	return payload + crc16CCITT(payload)
}

var (
	reVideoURL  = regexp.MustCompile(`https?://[^\s]+`)
	rePixEmail  = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	rePixNumber = regexp.MustCompile(`\d[\d.,-]*\d`)
)

func sanitizeOutgoingLinks(body string) (string, bool) {
	if body == "" {
		return body, false
	}
	changed := false
	updated := reVideoURL.ReplaceAllStringFunc(body, func(raw string) string {
		cleaned, didClean := stripTrackingParams(raw)
		if didClean {
			changed = true
			return cleaned
		}
		return raw
	})
	return updated, changed
}

func stripTrackingParams(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw, false
	}
	host := strings.ToLower(parsed.Hostname())
	isInstagram := strings.HasSuffix(host, "instagram.com")
	isYouTube := host == "youtu.be" || strings.HasSuffix(host, "youtube.com")
	if !isInstagram && !isYouTube {
		return raw, false
	}
	query := parsed.Query()
	changed := false
	if isInstagram {
		if _, ok := query["igsh"]; ok {
			query.Del("igsh")
			changed = true
		}
	}
	if isYouTube {
		if _, ok := query["si"]; ok {
			query.Del("si")
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), true
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func convertVideoToMP4(inputPath string) (string, error) {
	outputPath := strings.TrimSuffix(inputPath, path.Ext(inputPath)) + "_converted.mp4"
	const maxVideoSizeBytes = 50 * 1024 * 1024

	convertWithCRF := func(crf int) (int64, error) {
		args := []string{
			"-y",
			"-i", inputPath,
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", strconv.Itoa(crf),
			"-movflags", "+faststart",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-b:a", "96k",
			outputPath,
		}
		log.Printf("🎬 ffmpeg %v", args)
		cmd := exec.Command("ffmpeg", args...)
		out, err := cmd.CombinedOutput()
		if len(out) > 0 {
			log.Printf("ffmpeg output: %s", string(out))
		}
		if err != nil {
			return 0, fmt.Errorf("ffmpeg falhou: %w", err)
		}

		return fileSize(outputPath)
	}

	crfAttempts := []int{23, 28, 32}
	convertedSize, err := convertWithCRF(crfAttempts[0])
	if err != nil {
		return "", err
	}

	if convertedSize <= maxVideoSizeBytes {
		return outputPath, nil
	}

	log.Printf("⚠️ vídeo com %.2f MB após CRF %d; tentando reduzir para abaixo de 50 MB", float64(convertedSize)/(1024*1024), crfAttempts[0])

	for i, crf := range crfAttempts[1:] {
		convertedSize, err = convertWithCRF(crf)
		if err != nil {
			return "", err
		}

		if convertedSize <= maxVideoSizeBytes {
			return outputPath, nil
		}
		if i < len(crfAttempts)-2 {
			log.Printf("⚠️ vídeo ainda com %.2f MB; tentando CRF %d", float64(convertedSize)/(1024*1024), crfAttempts[i+2])
		}
	}

	log.Printf("⚠️ não consegui reduzir %s abaixo de 50 MB, enviando com o menor tamanho obtido", outputPath)
	return outputPath, nil
}

func videoDurationSeconds(inputPath string) (float64, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			log.Printf("ffprobe output: %s", string(out))
		}
		return 0, fmt.Errorf("falha ao ler duração do vídeo: %w", err)
	}

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("falha ao converter duração do vídeo: %w", err)
	}
	return duration, nil
}

func convertImageToStickerWebP(inputPath string) (string, error) {
	outputPath := strings.TrimSuffix(inputPath, path.Ext(inputPath)) + "_sticker.webp"
	qualityAttempts := []int{80, 70, 60, 50}
	for i, q := range qualityAttempts {
		args := []string{
			"-y",
			"-i", inputPath,
			"-vf", "scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos,format=rgba,pad=512:512:(512-iw)/2:(512-ih)/2:color=0x00000000",
			"-vcodec", "libwebp",
			"-lossless", "0",
			"-compression_level", "6",
			"-q:v", strconv.Itoa(q),
			"-preset", "picture",
			"-an",
			"-vsync", "0",
			"-frames:v", "1",
			outputPath,
		}
		log.Printf("🎨 ffmpeg %v", args)
		cmd := exec.Command("ffmpeg", args...)
		out, err := cmd.CombinedOutput()
		if len(out) > 0 {
			log.Printf("ffmpeg output: %s", string(out))
		}
		if err != nil {
			return "", fmt.Errorf("ffmpeg falhou: %w", err)
		}

		size, err := fileSize(outputPath)
		if err != nil {
			return "", fmt.Errorf("falha ao ler figurinha: %w", err)
		}
		if size <= maxStickerSizeBytes {
			return outputPath, nil
		}
		if i < len(qualityAttempts)-1 {
			log.Printf("⚠️ figurinha ficou com %.0f KB; reduzindo qualidade para %d", float64(size)/1024, qualityAttempts[i+1])
		}
	}

	return "", fmt.Errorf("figurinha ficou maior que o limite de %d KB", maxStickerSizeBytes/1024)
}

func convertVideoToStickerWebP(inputPath string) (string, error) {
	outputPath := strings.TrimSuffix(inputPath, path.Ext(inputPath)) + "_sticker.webp"
	duration, err := videoDurationSeconds(inputPath)
	if err != nil {
		return "", err
	}

	const maxDuration = 6.0
	if duration > maxDuration {
		duration = maxDuration
	}
	const minDuration = 0.8

	attempts := []struct {
		fps     int
		quality int
		qv      int
	}{
		{fps: 15, quality: 78, qv: 68},
		{fps: 12, quality: 72, qv: 62},
		{fps: 10, quality: 68, qv: 58},
	}

	for i, attempt := range attempts {
		attemptDuration := duration
		attemptQuality := attempt.quality
		attemptQV := attempt.qv
		qualityDrops := 0

		for {
			args := []string{
				"-y",
				"-i", inputPath,
				"-t", fmt.Sprintf("%.2f", attemptDuration),
				"-vf", fmt.Sprintf("scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos,fps=%d,format=rgba,pad=512:512:(512-iw)/2:(512-ih)/2:color=0x00000000", attempt.fps),
				"-loop", "0",
				"-an",
				"-vsync", "0",
				"-c:v", "libwebp",
				"-quality", strconv.Itoa(attemptQuality),
				"-compression_level", "6",
				"-q:v", strconv.Itoa(attemptQV),
				"-preset", "picture",
				outputPath,
			}
			log.Printf("🎞️ ffmpeg %v", args)
			cmd := exec.Command("ffmpeg", args...)
			out, err := cmd.CombinedOutput()
			if len(out) > 0 {
				log.Printf("ffmpeg output: %s", string(out))
			}
			if err != nil {
				return "", fmt.Errorf("ffmpeg falhou: %w", err)
			}

			size, err := fileSize(outputPath)
			if err != nil {
				return "", fmt.Errorf("falha ao ler figurinha animada: %w", err)
			}
			if size <= maxStickerSizeBytes {
				return outputPath, nil
			}

			if qualityDrops < 2 && attemptQuality > 50 {
				qualityDrops++
				attemptQuality = maxInt(attemptQuality-5, 50)
				attemptQV = maxInt(attemptQV-5, 40)
				log.Printf("⚠️ figurinha animada ficou com %.0f KB; reduzindo qualidade para q=%d qv=%d", float64(size)/1024, attemptQuality, attemptQV)
				continue
			}

			ratio := float64(maxStickerSizeBytes) / float64(size)
			reducedDuration := attemptDuration * ratio
			minimalStep := attemptDuration * 0.8
			if reducedDuration < minimalStep {
				reducedDuration = minimalStep
			}
			if reducedDuration < minDuration {
				log.Printf("⚠️ figurinha animada ficou com %.0f KB; duração mínima atingida (%.2fs)", float64(size)/1024, minDuration)
				break
			}
			if reducedDuration >= attemptDuration-0.05 {
				log.Printf("⚠️ figurinha animada ficou com %.0f KB; redução proporcional de duração seria mínima (%.2fs)", float64(size)/1024, reducedDuration)
				break
			}

			log.Printf("⚠️ figurinha animada ficou com %.0f KB; reduzindo duração para %.2fs (q=%d qv=%d fps=%d) e tentando novamente", float64(size)/1024, reducedDuration, attemptQuality, attemptQV, attempt.fps)
			attemptDuration = reducedDuration
		}

		if i < len(attempts)-1 {
			log.Printf("⚠️ figurinha animada ainda acima do limite; tentando configuração mais leve (fps %d)", attempts[i+1].fps)
		}
	}

	return "", fmt.Errorf("figurinha animada ficou maior que o limite de %d KB", maxStickerSizeBytes/1024)
}

func extractVideoURL(text string) string {
	match := reVideoURL.FindString(text)
	return match
}

func buildApiflashURL(targetURL string) (string, error) {
	key := strings.TrimSpace(os.Getenv("APIFLASH_KEY"))
	if key == "" {
		return "", fmt.Errorf("APIFLASH_KEY não configurado")
	}
	query := url.Values{}
	query.Set("access_key", key)
	query.Set("wait_until", "page_loaded")
	query.Set("url", targetURL)
	query.Set("no_ads", "true")
	query.Set("full_page", "true")
	query.Set("user_agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7_3 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1")
	query.Set("width", "990")
	return "https://api.apiflash.com/v1/urltoimage?" + query.Encode(), nil
}

func fetchApiflashScreenshot(ctx context.Context, targetURL string) ([]byte, string, error) {
	requestURL, err := buildApiflashURL(targetURL)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("falha ao criar requisição: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("falha ao chamar API do Apiflash: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("erro da API (%d): %s", resp.StatusCode, string(bodyBytes))
	}
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("falha ao ler imagem: %w", err)
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	return bytes, mimeType, nil
}

func cookieArgsForURL(url string) []string {
	cookiePath := ""
	switch {
	case strings.Contains(url, "instagram.com") || strings.Contains(url, "threads.net"):
		cookiePath = instaCookies
	case strings.Contains(url, "tiktok.com"):
		cookiePath = tiktokCookies
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

func downloadAndSendMedia(cli *whatsmeow.Client, chat string, url string, replyContext *waProto.ContextInfo) {
	tmpDir, err := os.MkdirTemp("", "vid-*")
	if err != nil {
		log.Printf("erro temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	tmpl := path.Join(tmpDir, "%(id)s.%(ext)s")
	cookieArgs := cookieArgsForURL(url)

	type downloadAttempt struct {
		name           string
		includeProxy   bool
		includeCookies bool
	}

	attempts := []downloadAttempt{
		{name: "proxy sem cookies", includeProxy: true, includeCookies: false},
		{name: "proxy com cookies", includeProxy: true, includeCookies: true},
		{name: "direto sem cookies", includeProxy: false, includeCookies: false},
		{name: "direto com cookies", includeProxy: false, includeCookies: true},
	}

	var files []os.DirEntry
	for _, attempt := range attempts {
		args := []string{}
		if attempt.includeProxy {
			if downloadProxy == "" {
				log.Printf("⚠️ proxy não configurado, pulando tentativa %s", attempt.name)
				continue
			}
			args = append(args, "--proxy", downloadProxy)
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
			if replyContext != nil {
				vidMsg.ContextInfo = replyContext
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
			if replyContext != nil {
				imgMsg.ContextInfo = replyContext
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

	var err error
	openaiClient = go_openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	pathMp3 = mustEnv("PATH_MP3", ".")
	sessionPath := mustEnv("PATH_SESSION", "./")
	userPhone := normalizePhone(mustEnv("USER_PHONE", ""))
	if userPhone == "" {
		log.Fatal("USER_PHONE não definido")
	}
	userJID = userPhone + "@s.whatsapp.net"

	model = mustEnv("MODEL", "gpt-4o-mini")
	promptSummary = mustEnv("PROMPT", "Faça um resumo das seguintes mensagens...")
	promptChatGPT = mustEnv("CHATGPT_PROMPT", "Responda ao questionamento a seguir...")
	instaCookies = mustEnv("INSTA_COOKIES_PATH", "./insta_cookies.txt")
	tiktokCookies = mustEnv("TIKTOK_COOKIES_PATH", "./tiktok_cookies.txt")
	downloadProxy = mustEnv("DOWNLOAD_PROXY", "")

	allowedGroups = make(map[string]bool)
	for _, g := range strings.Split(mustEnv("GROUPS", ""), ",") {
		if g != "" {
			allowedGroups[g] = true
		}
	}

	configureClientIdentity()

	triggerDB, err = initTriggerStore(sessionPath)
	if err != nil {
		log.Fatalf("erro ao abrir banco de gatilhos: %v", err)
	}

	dbLog := waLog.Stdout("DB", "ERROR", true)
	dsn := fmt.Sprintf("file:%s/datastore.db?_foreign_keys=on", sessionPath)
	ctx := context.Background()
	sqlContainer, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		log.Fatalf("erro ao abrir store: %v", err)
	}
	deviceStore, err := sqlContainer.GetFirstDevice(ctx)
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

	apiServer := startAPIServer(client)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("⚠️ falha ao encerrar API HTTP: %v", err)
	}
	client.Disconnect()
}

func handleMessage(cli *whatsmeow.Client, v *events.Message) {
	// JIDs
	senderFull := v.Info.Sender.String()
	senderBare := bareJID(senderFull)
	chatBare := bareJID(v.Info.Chat.String())
	senderJID := senderBare
	infoIsFromMe := v.Info.IsFromMe

	// ignora status e newsletters vazios
	if chatBare == "status@broadcast" || strings.HasSuffix(chatBare, "@newsletter") {
		return
	}

	// atualiza cache de nomes
	fromName := v.Info.PushName
	if fromName == "" {
		fromName = senderBare
	}
	contactNames[senderBare] = fromName

	if reaction := v.Message.GetReactionMessage(); reaction != nil {
		reactionText := reaction.GetText()
		targetID := ""
		targetChat := chatBare
		if key := reaction.GetKey(); key != nil {
			targetID = key.GetID()
			if remote := key.GetRemoteJID(); remote != "" {
				targetChat = bareJID(remote)
			}
		}
		log.Printf("😊 Reaction=%q from=%s chat=%s msgID=%s", reactionText, senderBare, targetChat, targetID)
		return
	}

	// 1) extrai texto
	body := v.Message.GetConversation()
	if ext := v.Message.GetExtendedTextMessage(); ext != nil {
		body = ext.GetText()
	}

	log.Printf("📥 DEBUG sender=%s chat=%s body=%q", senderBare, chatBare, body)

	trimmedBody := strings.TrimSpace(body)
	if isFromMe(senderJID, infoIsFromMe) && !v.IsEdit {
		if sanitized, changed := sanitizeOutgoingLinks(body); changed {
			replyContext := extractReplyContext(v.Message)
			if err := sendTextWithReply(cli, chatBare, sanitized, replyContext); err != nil {
				log.Printf("⚠️ falha ao reenviar link sanitizado: %v", err)
			} else {
				revokeTriggerMessage(cli, v.Info.Chat, v.Info.ID)
				log.Printf("✅ reenviou link sanitizado e removeu mensagem original")
			}
			body = sanitized
			trimmedBody = strings.TrimSpace(body)
		}
	}
	if strings.HasPrefix(trimmedBody, "!") {
		commandName := trimmedBody
		if idx := strings.IndexAny(trimmedBody, " \t\n"); idx != -1 {
			commandName = trimmedBody[:idx]
		}
		logTriggerEvaluation(commandName, chatBare, senderBare, senderFull, body, infoIsFromMe)
		triggerKeepAlive(cli)
	}

	// reset diário
	if time.Now().Day() != currentDay {
		messageHistory = make(map[string][]Msg)
		currentDay = time.Now().Day()
	}

	if aud := v.Message.GetAudioMessage(); aud != nil {
		data, err := cli.Download(context.Background(), aud)
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
	if isFromMe(senderJID, infoIsFromMe) {
		if trimmedBody == "!salvar" || strings.HasPrefix(trimmedBody, "!salvar ") {
			log.Println("✅ Disparou !salvar")
			triggerInput := strings.TrimSpace(trimmedBody[len("!salvar"):])
			if triggerInput == "" {
				sendText(cli, chatBare, "❌ Uso: responda a uma mensagem com !salvar <gatilho>.")
				return
			}
			triggerName, err := normalizeTriggerName(triggerInput)
			if err != nil {
				sendText(cli, chatBare, "❌ "+err.Error())
				return
			}
			if _, reserved := reservedTriggers[triggerName]; reserved {
				sendText(cli, chatBare, "❌ Esse gatilho é reservado.")
				return
			}
			ext := v.Message.GetExtendedTextMessage()
			if ext == nil || ext.GetContextInfo() == nil || ext.GetContextInfo().GetQuotedMessage() == nil {
				sendText(cli, chatBare, "❌ Responda a uma mensagem para usar !salvar.")
				return
			}
			qm := ext.GetContextInfo().GetQuotedMessage()
			trig, err := buildTriggerFromQuoted(cli, qm)
			if err != nil {
				sendText(cli, chatBare, "❌ "+err.Error())
				return
			}
			trig.Name = triggerName
			if err := saveTrigger(triggerDB, trig); err != nil {
				sendText(cli, chatBare, "❌ Falha ao salvar gatilho: "+err.Error())
				return
			}
			sendText(cli, chatBare, fmt.Sprintf("✅ Gatilho !%s salvo.", triggerName))
			return
		}
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
			return
		}
		if trimmedBody == "!pix" || strings.HasPrefix(trimmedBody, "!pix ") {
			log.Println("✅ Disparou !pix")
			amountInput := strings.TrimSpace(trimmedBody[len("!pix"):])
			amount := ""
			if amountInput != "" {
				amount = normalizePixAmount(amountInput)
				if amount == "" {
					sendText(cli, chatBare, "❌ Valor inválido para o Pix.")
					return
				}
			}
			email := sanitizePixValue(os.Getenv("EMAIL"))
			name := sanitizePixValue(os.Getenv("NOME"))
			city := sanitizePixValue(os.Getenv("CIDADE"))
			if email == "" || name == "" || city == "" {
				sendText(cli, chatBare, "❌ Configure EMAIL, NOME e CIDADE nas variáveis de ambiente.")
				return
			}
			payload := buildPixPayload(email, name, city, amount)
			sendText(cli, chatBare, payload)
			return
		}
		if trimmedBody == "!copia" {
			log.Println("✅ Disparou !copia")
			ext := v.Message.GetExtendedTextMessage()
			if ext == nil || ext.GetContextInfo() == nil || ext.GetContextInfo().GetQuotedMessage() == nil {
				sendText(cli, chatBare, "❌ Responda a uma mensagem para usar !copia.")
				return
			}
			qm := ext.GetContextInfo().GetQuotedMessage()
			quotedText := qm.GetConversation()
			if quotedText == "" && qm.GetExtendedTextMessage() != nil {
				quotedText = qm.GetExtendedTextMessage().GetText()
			}
			if strings.TrimSpace(quotedText) == "" {
				sendText(cli, chatBare, "❌ A mensagem respondida não tem texto.")
				return
			}
			seen := make(map[string]struct{})
			found := false
			for _, email := range rePixEmail.FindAllString(quotedText, -1) {
				if _, ok := seen[email]; ok {
					continue
				}
				seen[email] = struct{}{}
				sendText(cli, chatBare, email)
				found = true
			}
			strippedText := rePixEmail.ReplaceAllString(quotedText, " ")
			for _, number := range rePixNumber.FindAllString(strippedText, -1) {
				if _, ok := seen[number]; ok {
					continue
				}
				seen[number] = struct{}{}
				sendText(cli, chatBare, number)
				found = true
			}
			if !found {
				sendText(cli, chatBare, "❌ Nenhum e-mail ou número encontrado.")
			}
			return
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
							Text: promptChatGPT + "\n\n" + prompt,
						},
						{
							Type: go_openai.ChatMessagePartTypeImageURL,
							ImageURL: &go_openai.ChatMessageImageURL{
								URL: fmt.Sprintf("data:%s;base64,%s", quotedMime, base64.StdEncoding.EncodeToString(quotedImage)),
							},
						},
					}
				} else {
					message.Content = promptChatGPT + "\n\n" + prompt
				}
				req := go_openai.ChatCompletionRequest{
					Model:    model,
					Messages: []go_openai.ChatCompletionMessage{message},
				}
				if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err == nil {
					sendText(cli, chatBare, resp.Choices[0].Message.Content)
				}
			}
			return

		}
		if strings.HasPrefix(body, "!edit") {
			log.Println("✅ Disparou !edit")
			ext := v.Message.GetExtendedTextMessage()
			if ext == nil || ext.GetContextInfo() == nil || ext.GetContextInfo().GetQuotedMessage() == nil || ext.GetContextInfo().GetQuotedMessage().GetImageMessage() == nil {
				sendText(cli, chatBare, "❌ Responda a uma imagem para usar !edit.")
				return
			}
			prompt := strings.TrimSpace(body[len("!edit"):])
			if prompt == "" {
				sendText(cli, chatBare, "❌ Informe o prompt após !edit.")
				return
			}
			ctxInfo := ext.GetContextInfo()
			qm := ctxInfo.GetQuotedMessage()
			img := qm.GetImageMessage()
			imgBytes, err := cli.Download(context.Background(), img)
			if err != nil {
				sendText(cli, chatBare, "❌ Falha ao baixar a imagem: "+err.Error())
				return
			}

			imgDecoded, _, err := image.Decode(bytes.NewReader(imgBytes))
			if err != nil {
				sendText(cli, chatBare, "❌ Não consegui decodificar a imagem original: "+err.Error())
				return
			}

			var pngBuffer bytes.Buffer
			if err := png.Encode(&pngBuffer, imgDecoded); err != nil {
				sendText(cli, chatBare, "❌ Não consegui converter a imagem para PNG: "+err.Error())
				return
			}

			editedBytes, err := createImageEdit(context.Background(), pngBuffer.Bytes(), prompt)
			if err != nil {
				sendText(cli, chatBare, "❌ Erro ao editar imagem: "+err.Error())
				return
			}

			up, err := cli.Upload(context.Background(), editedBytes, whatsmeow.MediaImage)
			if err != nil {
				sendText(cli, chatBare, "❌ Erro no upload da imagem editada: "+err.Error())
				return
			}
			jid, err := types.ParseJID(chatBare)
			if err != nil {
				log.Printf("⚠️ JID inválido: %v", err)
				return
			}
			imageMsg := &waProto.ImageMessage{
				Caption:       proto.String(prompt),
				Mimetype:      proto.String("image/png"),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imageMsg}); err != nil {
				log.Printf("❌ falha ao enviar imagem editada: %v", err)
			}
			return

		}
		// !img
		if strings.HasPrefix(body, "!img ") {
			prompt := strings.TrimSpace(body[len("!img "):])
			log.Printf("🖼️ Gerando imagem para: %q", prompt)
			req := go_openai.ImageRequest{
				Prompt: prompt,
				N:      1,
				Model:  "gpt-image-1.5",
			}
			if size := strings.TrimSpace(os.Getenv("OPENAI_IMAGE_SIZE")); size != "" {
				req.Size = size
			}
			respImg, err := openaiClient.CreateImage(context.Background(), req)
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
		if trimmedBody == "!sticker" {
			log.Println("✅ Disparou !sticker")
			ext := v.Message.GetExtendedTextMessage()
			if ext == nil || ext.GetContextInfo() == nil || ext.GetContextInfo().GetQuotedMessage() == nil {
				sendText(cli, chatBare, "❌ Responda a uma imagem ou vídeo curto para usar !sticker.")
				return
			}
			ctx := ext.GetContextInfo()
			qm := ctx.GetQuotedMessage()

			tmpDir, err := os.MkdirTemp("", "sticker-*")
			if err != nil {
				sendText(cli, chatBare, "❌ Não consegui criar pasta temporária: "+err.Error())
				return
			}
			defer os.RemoveAll(tmpDir)

			var (
				stickerPath string
				isAnimated  bool
			)

			switch {
			case qm.GetImageMessage() != nil:
				img := qm.GetImageMessage()
				data, err := cli.Download(context.Background(), img)
				if err != nil {
					sendText(cli, chatBare, "❌ Falha ao baixar a imagem: "+err.Error())
					return
				}
				ext := ".img"
				if mt := img.GetMimetype(); mt != "" {
					if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
						ext = exts[0]
					}
				}
				inputPath := path.Join(tmpDir, "quoted"+ext)
				if err := os.WriteFile(inputPath, data, 0600); err != nil {
					sendText(cli, chatBare, "❌ Falha ao salvar a imagem: "+err.Error())
					return
				}
				stickerPath, err = convertImageToStickerWebP(inputPath)
				if err != nil {
					sendText(cli, chatBare, "❌ Erro ao converter figurinha: "+err.Error())
					return
				}
			case qm.GetVideoMessage() != nil:
				vid := qm.GetVideoMessage()
				data, err := cli.Download(context.Background(), vid)
				if err != nil {
					sendText(cli, chatBare, "❌ Falha ao baixar o vídeo: "+err.Error())
					return
				}
				ext := ".mp4"
				if mt := vid.GetMimetype(); mt != "" {
					if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
						ext = exts[0]
					}
				}
				inputPath := path.Join(tmpDir, "quoted"+ext)
				if err := os.WriteFile(inputPath, data, 0600); err != nil {
					sendText(cli, chatBare, "❌ Falha ao salvar o vídeo: "+err.Error())
					return
				}
				stickerPath, err = convertVideoToStickerWebP(inputPath)
				if err != nil {
					sendText(cli, chatBare, "❌ Erro ao converter figurinha animada: "+err.Error())
					return
				}
				isAnimated = true
			default:
				sendText(cli, chatBare, "❌ Responda a uma imagem ou vídeo curto para usar !sticker.")
				return
			}

			stickerBytes, err := os.ReadFile(stickerPath)
			if err != nil {
				sendText(cli, chatBare, "❌ Não consegui ler a figurinha: "+err.Error())
				return
			}
			up, err := cli.Upload(context.Background(), stickerBytes, whatsmeow.MediaImage)
			if err != nil {
				sendText(cli, chatBare, "❌ Erro no upload da figurinha: "+err.Error())
				return
			}
			jid, err := types.ParseJID(chatBare)
			if err != nil {
				log.Printf("⚠️ JID inválido: %v", err)
				return
			}
			stickerMsg := &waProto.StickerMessage{
				Mimetype:      proto.String("image/webp"),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				Height:        proto.Uint32(512),
				Width:         proto.Uint32(512),
			}
			if isAnimated {
				stickerMsg.IsAnimated = proto.Bool(true)
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{StickerMessage: stickerMsg}); err != nil {
				sendText(cli, chatBare, "❌ Falha ao enviar figurinha: "+err.Error())
			} else {
				log.Printf("✅ Figurinha enviada para %s", chatBare)
			}
			return
		}
		if body == "!download" {
			log.Println("✅ Disparou !download")
			var quotedText string
			replyContext := extractReplyContext(v.Message)
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
				go downloadAndSendMedia(cli, chatBare, url, replyContext)
				revokeTriggerMessage(cli, v.Info.Chat, v.Info.ID)
			} else {
				if err := sendTextWithReply(cli, chatBare, "❌ Link inválido para download.", replyContext); err != nil {
					log.Printf("❌ falha ao enviar erro do !download: %v", err)
				}
			}
			return
		}
		if body == "!print" {
			log.Println("✅ Disparou !print")
			var quotedText string
			replyContext := extractReplyContext(v.Message)
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
				sendText(cli, chatBare, "❌ Responda ao link para usar !print.")
				return
			}
			targetURL := extractVideoURL(quotedText)
			if targetURL == "" {
				if err := sendTextWithReply(cli, chatBare, "❌ Link inválido para !print.", replyContext); err != nil {
					log.Printf("❌ falha ao enviar erro do !print: %v", err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			imgBytes, mimeType, err := fetchApiflashScreenshot(ctx, targetURL)
			if err != nil {
				if err := sendTextWithReply(cli, chatBare, "❌ Erro ao gerar captura: "+err.Error(), replyContext); err != nil {
					log.Printf("❌ falha ao enviar erro do !print: %v", err)
				}
				return
			}
			up, err := cli.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
			if err != nil {
				if err := sendTextWithReply(cli, chatBare, "❌ Erro no upload da captura: "+err.Error(), replyContext); err != nil {
					log.Printf("❌ falha ao enviar erro do !print: %v", err)
				}
				return
			}
			jid, err := types.ParseJID(chatBare)
			if err != nil {
				log.Printf("⚠️ JID inválido: %v", err)
				return
			}
			imageMsg := &waProto.ImageMessage{
				Mimetype:      proto.String(mimeType),
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
			}
			if replyContext != nil {
				imageMsg.ContextInfo = replyContext
			}
			if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imageMsg}); err != nil {
				log.Printf("❌ falha ao enviar captura: %v", err)
			} else {
				revokeTriggerMessage(cli, v.Info.Chat, v.Info.ID)
			}
			return
		}
		if body == "!paywall" {
			log.Println("✅ Disparou !paywall")
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
				sendText(cli, chatBare, "❌ Responda ao link para usar !paywall.")
				return
			}
			targetURL := extractVideoURL(quotedText)
			if targetURL == "" {
				sendText(cli, chatBare, "❌ Link inválido para !paywall.")
				return
			}
			base := strings.TrimSpace(os.Getenv("PAYWALL_REMOVER"))
			if base == "" {
				sendText(cli, chatBare, "❌ PAYWALL_REMOVER não configurado.")
				return
			}
			paywallURL := strings.TrimRight(base, "/") + "/" + targetURL
			sendText(cli, chatBare, paywallURL)
			return
		}
		// !ler
		if body == "!ler" {
			log.Println("✅ Disparou !ler")
			if ext := v.Message.GetExtendedTextMessage(); ext != nil {
				if ctx := ext.GetContextInfo(); ctx != nil {
					if qm := ctx.GetQuotedMessage(); qm != nil && qm.GetAudioMessage() != nil {
						replyContext := ctx
						aud := qm.GetAudioMessage()
						exts, _ := mime.ExtensionsByType(aud.GetMimetype())
						ext := ".ogg"
						if len(exts) > 0 {
							ext = exts[0]
						}
						orig := ctx.GetStanzaID()
						filePath := path.Join(pathMp3, orig+ext)
						tr, err := openaiClient.CreateTranscription(
							context.Background(),
							go_openai.AudioRequest{Model: go_openai.Whisper1, FilePath: filePath},
						)
						if err != nil {
							if err := sendTextWithReply(cli, chatBare, "❌ Erro na transcrição: "+err.Error(), replyContext); err != nil {
								log.Printf("❌ falha ao enviar erro do !ler: %v", err)
							}
						} else {
							if err := sendTextWithReply(cli, chatBare, "🗣️ "+tr.Text, replyContext); err != nil {
								log.Printf("❌ falha ao enviar resposta do !ler: %v", err)
							}
						}
						revokeTriggerMessage(cli, v.Info.Chat, v.Info.ID)
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
						filePath := path.Join(pathMp3, orig+ext)
						tr, err := openaiClient.CreateTranscription(
							context.Background(),
							go_openai.AudioRequest{Model: go_openai.Whisper1, FilePath: filePath},
						)
						if err != nil {
							sendText(cli, chatBare, "❌ Erro na transcrição: "+err.Error())
						} else {
							req := go_openai.ChatCompletionRequest{
								Model: model,
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
							if resp, err := openaiClient.CreateChatCompletion(context.Background(), req); err != nil {
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
		if strings.HasPrefix(trimmedBody, "!") && strings.IndexAny(trimmedBody, " \t\n") == -1 {
			triggerName, err := normalizeTriggerName(trimmedBody)
			if err == nil {
				if _, reserved := reservedTriggers[triggerName]; !reserved {
					trig, found, err := loadTrigger(triggerDB, triggerName)
					if err != nil {
						sendText(cli, chatBare, "❌ Falha ao carregar gatilho: "+err.Error())
						return
					}
					if found {
						replyContext := extractReplyContext(v.Message)
						if err := sendSavedTrigger(cli, chatBare, trig, replyContext); err != nil {
							sendText(cli, chatBare, "❌ Falha ao enviar gatilho: "+err.Error())
						}
						if !isPrivateChat(chatBare) && triggerName != "chatgpt" {
							revokeTriggerMessage(cli, v.Info.Chat, v.Info.ID)
						}
						return
					}
				}
			}
		}
	}

	// ==== comando !resumo (antes de gravar) ====
	if isAuthorizedGroup(chatBare) && isFromMe(senderJID, infoIsFromMe) && body == "!resumo" {
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
	if isFromMe(senderJID, infoIsFromMe) && isPrivateChat(chatBare) {
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
		if body == "!triggers" {
			triggers, err := listTriggers(triggerDB)
			if err != nil {
				sendText(cli, chatBare, "❌ Falha ao listar gatilhos: "+err.Error())
				return
			}
			if len(triggers) == 0 {
				sendText(cli, chatBare, "Nenhum gatilho salvo.")
				return
			}
			for i, name := range triggers {
				triggers[i] = "!" + name
			}
			sendText(cli, chatBare, "Gatilhos salvos: "+strings.Join(triggers, ", "))
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
	if err := sendTextWithError(cli, to, text); err != nil {
		log.Printf("❌ falha ao enviar mensagem: %v", err)
	}
}

func sendTextWithError(cli *whatsmeow.Client, to, text string) error {
	return sendTextWithReply(cli, to, text, nil)
}

func sendTextWithReply(cli *whatsmeow.Client, to, text string, replyContext *waProto.ContextInfo) error {
	jid, err := types.ParseJID(to)
	if err != nil {
		return fmt.Errorf("JID inválido %q: %w", to, err)
	}
	var msg *waProto.Message
	if replyContext != nil {
		msg = &waProto.Message{
			ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text:        proto.String(text),
				ContextInfo: replyContext,
			},
		}
	} else {
		msg = &waProto.Message{Conversation: proto.String(text)}
	}
	if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
		return fmt.Errorf("falha ao enviar mensagem: %w", err)
	}
	return nil
}

func extractReplyContext(msg *waProto.Message) *waProto.ContextInfo {
	if msg == nil {
		return nil
	}
	ext := msg.GetExtendedTextMessage()
	if ext == nil {
		return nil
	}
	ctx := ext.GetContextInfo()
	if ctx == nil || ctx.GetQuotedMessage() == nil {
		return nil
	}
	return ctx
}

func revokeTriggerMessage(cli *whatsmeow.Client, chat types.JID, messageID types.MessageID) {
	if _, err := cli.RevokeMessage(context.Background(), chat, messageID); err != nil {
		log.Printf("⚠️ falha ao apagar gatilho: %v", err)
	}
}

func normalizeTarget(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", fmt.Errorf("destino vazio")
	}
	if strings.Contains(trimmed, "@") {
		return trimmed, nil
	}
	return trimmed + "@s.whatsapp.net", nil
}

func normalizeTriggerName(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", fmt.Errorf("gatilho vazio")
	}
	if strings.HasPrefix(trimmed, "!") {
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	if trimmed == "" {
		return "", fmt.Errorf("gatilho vazio")
	}
	if strings.ContainsAny(trimmed, " \t\n") {
		return "", fmt.Errorf("gatilho precisa ser uma palavra")
	}
	return strings.ToLower(trimmed), nil
}

func initTriggerStore(sessionPath string) (*sql.DB, error) {
	dbPath := fmt.Sprintf("file:%s/triggers.db?_foreign_keys=on", sessionPath)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("abrindo banco de gatilhos: %w", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS saved_triggers (
			name TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			mime_type TEXT,
			file_name TEXT,
			data BLOB,
			text TEXT,
			caption TEXT,
			is_ptt INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("criando tabela de gatilhos: %w", err)
	}
	return db, nil
}

func saveTrigger(db *sql.DB, trig savedTrigger) error {
	_, err := db.Exec(`
		INSERT INTO saved_triggers (name, kind, mime_type, file_name, data, text, caption, is_ptt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind=excluded.kind,
			mime_type=excluded.mime_type,
			file_name=excluded.file_name,
			data=excluded.data,
			text=excluded.text,
			caption=excluded.caption,
			is_ptt=excluded.is_ptt
	`, trig.Name, trig.Kind, trig.MimeType, trig.FileName, trig.Data, trig.Text, trig.Caption, boolToInt(trig.IsPTT))
	if err != nil {
		return fmt.Errorf("salvando gatilho: %w", err)
	}
	return nil
}

func loadTrigger(db *sql.DB, name string) (savedTrigger, bool, error) {
	row := db.QueryRow(`
		SELECT name, kind, mime_type, file_name, data, text, caption, is_ptt
		FROM saved_triggers
		WHERE name = ?
	`, name)
	var trig savedTrigger
	var isPTT int
	if err := row.Scan(&trig.Name, &trig.Kind, &trig.MimeType, &trig.FileName, &trig.Data, &trig.Text, &trig.Caption, &isPTT); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return savedTrigger{}, false, nil
		}
		return savedTrigger{}, false, fmt.Errorf("carregando gatilho: %w", err)
	}
	trig.IsPTT = isPTT == 1
	return trig, true, nil
}

func listTriggers(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM saved_triggers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listando gatilhos: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("lendo gatilho: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterando gatilhos: %w", err)
	}
	return names, nil
}

func boolToInt(val bool) int {
	if val {
		return 1
	}
	return 0
}

func buildTriggerFromQuoted(cli *whatsmeow.Client, qm *waProto.Message) (savedTrigger, error) {
	if qm == nil {
		return savedTrigger{}, fmt.Errorf("mensagem citada vazia")
	}
	if text := strings.TrimSpace(qm.GetConversation()); text != "" {
		return savedTrigger{Kind: "text", Text: text}, nil
	}
	if ext := qm.GetExtendedTextMessage(); ext != nil && strings.TrimSpace(ext.GetText()) != "" {
		return savedTrigger{Kind: "text", Text: strings.TrimSpace(ext.GetText())}, nil
	}
	if img := qm.GetImageMessage(); img != nil {
		data, err := cli.Download(context.Background(), img)
		if err != nil {
			return savedTrigger{}, fmt.Errorf("falha ao baixar imagem: %w", err)
		}
		mimeType := img.GetMimetype()
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		return savedTrigger{
			Kind:     "image",
			MimeType: mimeType,
			Data:     data,
			Caption:  strings.TrimSpace(img.GetCaption()),
		}, nil
	}
	if vid := qm.GetVideoMessage(); vid != nil {
		data, err := cli.Download(context.Background(), vid)
		if err != nil {
			return savedTrigger{}, fmt.Errorf("falha ao baixar vídeo: %w", err)
		}
		mimeType := vid.GetMimetype()
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		return savedTrigger{
			Kind:     "video",
			MimeType: mimeType,
			Data:     data,
			Caption:  strings.TrimSpace(vid.GetCaption()),
		}, nil
	}
	if aud := qm.GetAudioMessage(); aud != nil {
		data, err := cli.Download(context.Background(), aud)
		if err != nil {
			return savedTrigger{}, fmt.Errorf("falha ao baixar áudio: %w", err)
		}
		mimeType := aud.GetMimetype()
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		return savedTrigger{
			Kind:     "audio",
			MimeType: mimeType,
			Data:     data,
			IsPTT:    aud.GetPTT(),
		}, nil
	}
	if doc := qm.GetDocumentMessage(); doc != nil {
		data, err := cli.Download(context.Background(), doc)
		if err != nil {
			return savedTrigger{}, fmt.Errorf("falha ao baixar documento: %w", err)
		}
		mimeType := doc.GetMimetype()
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		fileName := strings.TrimSpace(doc.GetFileName())
		return savedTrigger{
			Kind:     "document",
			MimeType: mimeType,
			FileName: fileName,
			Data:     data,
		}, nil
	}
	if sticker := qm.GetStickerMessage(); sticker != nil {
		data, err := cli.Download(context.Background(), sticker)
		if err != nil {
			return savedTrigger{}, fmt.Errorf("falha ao baixar figurinha: %w", err)
		}
		mimeType := sticker.GetMimetype()
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		return savedTrigger{
			Kind:     "sticker",
			MimeType: mimeType,
			Data:     data,
		}, nil
	}
	return savedTrigger{}, fmt.Errorf("tipo de mensagem não suportado")
}

func sendSavedTrigger(cli *whatsmeow.Client, chat string, trig savedTrigger, replyContext *waProto.ContextInfo) error {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return fmt.Errorf("JID inválido %q: %w", chat, err)
	}
	switch trig.Kind {
	case "text":
		if strings.TrimSpace(trig.Text) == "" {
			return fmt.Errorf("gatilho sem texto")
		}
		var msg *waProto.Message
		if replyContext != nil {
			msg = &waProto.Message{
				ExtendedTextMessage: &waProto.ExtendedTextMessage{
					Text:        proto.String(trig.Text),
					ContextInfo: replyContext,
				},
			}
		} else {
			msg = &waProto.Message{Conversation: proto.String(trig.Text)}
		}
		if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
			return fmt.Errorf("falha ao enviar mensagem: %w", err)
		}
		return nil
	case "image":
		mimeType := trig.MimeType
		if mimeType == "" {
			mimeType = http.DetectContentType(trig.Data)
		}
		up, err := cli.Upload(context.Background(), trig.Data, whatsmeow.MediaImage)
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
		if trig.Caption != "" {
			imgMsg.Caption = proto.String(trig.Caption)
		}
		if replyContext != nil {
			imgMsg.ContextInfo = replyContext
		}
		if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{ImageMessage: imgMsg}); err != nil {
			return fmt.Errorf("falha ao enviar imagem: %w", err)
		}
		return nil
	case "video":
		mimeType := trig.MimeType
		if mimeType == "" {
			mimeType = http.DetectContentType(trig.Data)
		}
		up, err := cli.Upload(context.Background(), trig.Data, whatsmeow.MediaVideo)
		if err != nil {
			return fmt.Errorf("upload do vídeo falhou: %w", err)
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
		if trig.Caption != "" {
			vidMsg.Caption = proto.String(trig.Caption)
		}
		if replyContext != nil {
			vidMsg.ContextInfo = replyContext
		}
		if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{VideoMessage: vidMsg}); err != nil {
			return fmt.Errorf("falha ao enviar vídeo: %w", err)
		}
		return nil
	case "audio":
		mimeType := trig.MimeType
		if mimeType == "" {
			mimeType = http.DetectContentType(trig.Data)
		}
		up, err := cli.Upload(context.Background(), trig.Data, whatsmeow.MediaAudio)
		if err != nil {
			return fmt.Errorf("upload do áudio falhou: %w", err)
		}
		audMsg := &waProto.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}
		if trig.IsPTT {
			audMsg.PTT = proto.Bool(true)
		}
		if replyContext != nil {
			audMsg.ContextInfo = replyContext
		}
		if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{AudioMessage: audMsg}); err != nil {
			return fmt.Errorf("falha ao enviar áudio: %w", err)
		}
		return nil
	case "document":
		mimeType := trig.MimeType
		if mimeType == "" {
			mimeType = http.DetectContentType(trig.Data)
		}
		fileName := strings.TrimSpace(trig.FileName)
		if fileName == "" {
			fileName = "documento"
		}
		up, err := cli.Upload(context.Background(), trig.Data, whatsmeow.MediaDocument)
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
			FileName:      proto.String(fileName),
		}
		if replyContext != nil {
			docMsg.ContextInfo = replyContext
		}
		if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{DocumentMessage: docMsg}); err != nil {
			return fmt.Errorf("falha ao enviar documento: %w", err)
		}
		return nil
	case "sticker":
		mimeType := trig.MimeType
		if mimeType == "" {
			mimeType = "image/webp"
		}
		up, err := cli.Upload(context.Background(), trig.Data, whatsmeow.MediaImage)
		if err != nil {
			return fmt.Errorf("upload da figurinha falhou: %w", err)
		}
		stickerMsg := &waProto.StickerMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}
		if replyContext != nil {
			stickerMsg.ContextInfo = replyContext
		}
		if _, err := cli.SendMessage(context.Background(), jid, &waProto.Message{StickerMessage: stickerMsg}); err != nil {
			return fmt.Errorf("falha ao enviar figurinha: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("tipo de gatilho desconhecido: %s", trig.Kind)
	}
}

func startAPIServer(cli *whatsmeow.Client) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "JSON inválido", http.StatusBadRequest)
			return
		}

		target, err := normalizeTarget(payload.To)
		if err != nil {
			http.Error(w, "Destino obrigatório", http.StatusBadRequest)
			return
		}

		text := strings.TrimSpace(payload.Text)
		if text == "" {
			http.Error(w, "Texto obrigatório", http.StatusBadRequest)
			return
		}

		if err := sendTextWithError(cli, target, text); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "queued",
		})
	})

	server := &http.Server{
		Addr:    ":9999",
		Handler: mux,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("falha ao iniciar API HTTP: %v", err)
		}
	}()
	log.Println("🌐 API REST pronta em http://0.0.0.0:9999")
	return server
}

func main() {}
