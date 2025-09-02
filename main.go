package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
)

// Subtitle model
type SubtitleEntry struct {
	Start float64
	End   float64
	Text  string
}

// JSON Transcript structure
type TranscriptSegment struct {
	ID               int     `json:"id"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

type TranscriptResponse struct {
	Text     string              `json:"text"`
	Language string              `json:"language"`
	Duration float64             `json:"duration"`
	Segments []TranscriptSegment `json:"segments"`
}

// Parser
type SubtitleParser struct{}

func (sp *SubtitleParser) parseTime(hours, minutes, seconds, milliseconds string) float64 {
	h, _ := strconv.Atoi(hours)
	m, _ := strconv.Atoi(minutes)
	s, _ := strconv.Atoi(seconds)
	ms, _ := strconv.Atoi(milliseconds)
	return float64(h*3600+m*60+s) + float64(ms)/1000.0
}

// Searcher
type SearchService struct{}

func (ss *SearchService) FindInSubtitles(subtitles []SubtitleEntry, keyword string) (float64, bool) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	for _, sub := range subtitles {
		text := strings.ToLower(sub.Text)
		if strings.Contains(text, keyword) {
			return sub.Start, true
		}
	}
	return 0, false
}

// App
type App struct {
	parser   *SubtitleParser
	searcher *SearchService
}

// New App
func NewApp() *App {
	return &App{
		parser:   &SubtitleParser{},
		searcher: &SearchService{},
	}
}

func (app *App) SearchKeywordInSubtitles(videoURL, keyword string, lang string) (float64, bool, string, error) {

	// Choose language: use provided language; default to en
	langCode := strings.ToLower(strings.TrimSpace(lang))
	if langCode == "" {
		langCode = "en"
	}

	// Use a unique output template to avoid file conflicts
	outputTemplate := "temp_subs"
	lowerKeyword := strings.ToLower(keyword)

	cmd := exec.Command("yt-dlp",
		"--skip-download",
		"--write-subs",
		"--write-auto-subs",
		"--sub-langs", langCode,
		"--sub-format", "srt/best",
		"--convert-subs", "srt",
		"-o", outputTemplate,
		videoURL,
	)
	srtFileName := fmt.Sprintf("%s.%s.srt", outputTemplate, langCode)
	output, err := cmd.CombinedOutput()
	log.Printf("commandt: %s", string(output))
	srtContent, errFile := os.ReadFile(srtFileName)

	if errFile != nil {
		// Fast path: transcribe chunks sequentially and return early on first match
		if ts, ok, err := TranscribeChunkedUntilMatch(videoURL, keyword); err == nil && ok {
			return ts, true, langCode, nil
		} else if err != nil {
			log.Printf("early chunked transcription failed: %v", err)
		}

		transcriptFile, err := GetTranscript(videoURL)
		if err != nil {
			return 0, false, langCode, fmt.Errorf("failed to get transcript: %w", err)
		}

		transcriptContent, err := os.ReadFile(transcriptFile)
		if err != nil {
			return 0, false, langCode, fmt.Errorf("failed to read transcript file: %w", err)
		}

		// Check if it's a JSON file
		if strings.HasSuffix(transcriptFile, ".json") {
			if ts, ok, err := searchInTranscriptJSON(transcriptFile, keyword); err == nil && ok {
				return ts, true, langCode, nil
			} else if err != nil {
				return 0, false, langCode, fmt.Errorf("failed to parse JSON transcript: %w", err)
			}
		} else {
			// Search in plain text transcript
			transcriptText := string(transcriptContent)
			lowerTranscript := strings.ToLower(transcriptText)
			if strings.Contains(lowerTranscript, lowerKeyword) {
				wordsBeforeKeyword := countWordsBeforeKeyword(transcriptText, keyword)
				estimatedTime := float64(wordsBeforeKeyword) / 150.0 * 60.0 // Convert to seconds
				return estimatedTime, true, langCode, nil
			}
		}

		// Clean up transcript file
		defer os.Remove(transcriptFile)
		return 0, false, langCode, nil
	}

	// Read SRT file content

	if err != nil {
		return 0, false, langCode, fmt.Errorf("failed to read SRT file: %w", err)
	}

	// Optionally, clean up the SRT file after reading
	defer os.Remove(srtFileName)
	subs, err := app.parser.ParseSRTContent(string(srtContent))
	if err != nil {
		return 0, false, langCode, fmt.Errorf("failed to parse SRT subtitles: %w", err)
	}

	for _, sub := range subs {
		if strings.Contains(strings.ToLower(sub.Text), lowerKeyword) {
			return sub.Start, true, langCode, nil
		}
	}
	return 0, false, langCode, nil
}

// TranscribeChunkedUntilMatch downloads audio, splits into 5-min chunks, and transcribes chunks in order.
// Returns immediately when keyword is found with absolute timestamp; otherwise returns not found after all chunks.
func TranscribeChunkedUntilMatch(videoURL, keyword string) (float64, bool, error) {
	// Download audio (same settings as GetTranscript)
	audioFile := "audio.%(ext)s"
	cmdAudio := exec.Command("yt-dlp",
		"-f", "bestaudio",
		"--extract-audio",
		"--audio-format", "mp3",
		"--audio-quality", "32K",
		"--postprocessor-args", "ffmpeg:-ac 1 -ar 8000",
		"-o", audioFile,
		videoURL,
	)
	if out, err := cmdAudio.CombinedOutput(); err != nil {
		log.Printf("yt-dlp audio download error: %s", string(out))
		return 0, false, fmt.Errorf("audio download failed: %w", err)
	}
	audioFileName := "audio.mp3"

	// Segment to chunks
	chunksDir := "chunks_early"
	_ = os.RemoveAll(chunksDir)
	if err := os.MkdirAll(chunksDir, 0755); err != nil {
		return 0, false, fmt.Errorf("failed to create chunks dir: %w", err)
	}
	chunkDurationSec := 300
	chunkPattern := filepath.Join(chunksDir, "chunk_%03d.mp3")
	segCmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", audioFileName,
		"-ar", "16000",
		"-ac", "1",
		"-b:a", "32k",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", chunkDurationSec),
		"-reset_timestamps", "1",
		"-y", chunkPattern,
	)
	if out, err := segCmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg segment error: %s", string(out))
		_ = os.Remove(audioFileName)
		_ = os.RemoveAll(chunksDir)
		return 0, false, fmt.Errorf("failed to segment audio: %w", err)
	}
	chunkFiles, err := filepath.Glob(filepath.Join(chunksDir, "chunk_*.mp3"))
	if err != nil || len(chunkFiles) == 0 {
		_ = os.Remove(audioFileName)
		_ = os.RemoveAll(chunksDir)
		return 0, false, fmt.Errorf("no chunks produced: %w", err)
	}
	sort.Strings(chunkFiles)

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		_ = os.Remove(audioFileName)
		_ = os.RemoveAll(chunksDir)
		return 0, false, fmt.Errorf("OPENAI_API_KEY not set")
	}
	client := openai.NewClient(apiKey)
	lowerKeyword := strings.ToLower(strings.TrimSpace(keyword))

	for i, file := range chunkFiles {
		resp, err := client.CreateTranscription(
			context.Background(),
			openai.AudioRequest{
				Model:    openai.Whisper1,
				FilePath: file,
				Format:   openai.AudioResponseFormatVerboseJSON,
				TimestampGranularities: []openai.TranscriptionTimestampGranularity{
					openai.TranscriptionTimestampGranularitySegment,
				},
			},
		)
		if err != nil {
			// Continue on error to try next chunk, but log it
			log.Printf("transcription error on chunk %d: %v", i, err)
			continue
		}
		offset := float64(i * chunkDurationSec)
		for _, s := range resp.Segments {
			if strings.Contains(strings.ToLower(s.Text), lowerKeyword) {
				_ = os.Remove(audioFileName)
				_ = os.RemoveAll(chunksDir)
				return s.Start + offset, true, nil
			}
		}
	}

	_ = os.Remove(audioFileName)
	_ = os.RemoveAll(chunksDir)
	return 0, false, nil
}
func (sp *SubtitleParser) ParseSRTContent(content string) ([]SubtitleEntry, error) {
	var entries []SubtitleEntry
	blocks := strings.Split(content, "\n\n")
	timeRegex := regexp.MustCompile(`(\d{2}):(\d{2}):(\d{2}),(\d{3})\s*-->\s*(\d{2}):(\d{2}):(\d{2}),(\d{3})`)

	for _, block := range blocks {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}

		timelineIdx := -1
		for i, line := range lines {
			if timeRegex.MatchString(line) {
				timelineIdx = i
				break
			}
		}
		if timelineIdx == -1 {
			continue
		}

		matches := timeRegex.FindStringSubmatch(lines[timelineIdx])
		if len(matches) != 9 {
			continue
		}

		start := sp.parseTime(matches[1], matches[2], matches[3], matches[4])
		end := sp.parseTime(matches[5], matches[6], matches[7], matches[8])

		var textParts []string
		for i := timelineIdx + 1; i < len(lines); i++ {
			if text := strings.TrimSpace(lines[i]); text != "" {
				text = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, "")
				textParts = append(textParts, text)
			}
		}

		if len(textParts) > 0 {
			entries = append(entries, SubtitleEntry{
				Start: start,
				End:   end,
				Text:  strings.Join(textParts, " "),
			})
		}
	}

	return entries, nil
}

// HTTP Handlers
type SearchRequest struct {
	VideoURL string `json:"video_url"`
	Keyword  string `json:"keyword"`
	Language string `json:"language,omitempty"`
}

type SearchResponse struct {
	Found    bool   `json:"found"`
	Time     string `json:"time"`
	Source   string `json:"source"`
	Language string `json:"language,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func (app *App) searchHandler(c *gin.Context) {

	var req SearchRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{Error: "Invalid JSON request"})
		return
	}

	if req.VideoURL == "" || req.Keyword == "" {
		c.JSON(400, ErrorResponse{Error: "videourl and keyword are required"})
		return
	}

	timestamp, found, usedLang, err := app.SearchKeywordInSubtitles(req.VideoURL, req.Keyword, req.Language)
	if err != nil {
		c.JSON(500, ErrorResponse{Error: err.Error()})
		return
	}

	resp := SearchResponse{
		Found:    found,
		Time:     "",
		Source:   "subtitles",
		Language: usedLang,
	}
	if found {
		resp.Time = secondsToTimeString(timestamp)
	}
	c.JSON(200, resp)
}

// Helper to format seconds as HH:MM:SS
func secondsToTimeString(seconds float64) string {
	totalSeconds := int(seconds + 0.5) // round to nearest second
	h := totalSeconds / 3600
	m := (totalSeconds % 3600) / 60
	s := totalSeconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
func searchInTranscriptJSON(filePath, keyword string) (float64, bool, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	lowerKeyword := strings.ToLower(strings.TrimSpace(keyword))
	var fullText string

	// Expect a JSON object at the top level
	tok, err := dec.Token()
	if err != nil {
		return 0, false, err
	}
	if _, ok := tok.(json.Delim); !ok {
		return 0, false, fmt.Errorf("invalid JSON transcript format")
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, false, err
		}
		key, _ := keyTok.(string)
		switch key {
		case "segments":
			// Start of segments array
			if _, err := dec.Token(); err != nil { // should be '['
				return 0, false, err
			}
			for dec.More() {
				var seg TranscriptSegment
				if err := dec.Decode(&seg); err != nil {
					return 0, false, err
				}
				if strings.Contains(strings.ToLower(seg.Text), lowerKeyword) {
					return seg.Start, true, nil
				}
			}
			// consume closing ']'
			if _, err := dec.Token(); err != nil {
				return 0, false, err
			}
		default:
			// For other fields, we may want the top-level "text" for fallback
			if key == "text" {
				var v string
				if err := dec.Decode(&v); err != nil {
					return 0, false, err
				}
				fullText = v
				continue
			}
			// Skip value for keys we're not using
			var skip interface{}
			if err := dec.Decode(&skip); err != nil {
				return 0, false, err
			}
		}
	}

	// Fallback: search in full text if available
	if fullText != "" && strings.Contains(strings.ToLower(fullText), lowerKeyword) {
		wordsBeforeKeyword := countWordsBeforeKeyword(fullText, keyword)
		estimatedTime := float64(wordsBeforeKeyword) / 150.0 * 60.0
		return estimatedTime, true, nil
	}

	return 0, false, nil
}
func GetTranscript(videoURL string) (string, error) {
	// outputTemplate := "temp_subs_check"

	// // 1️⃣ تحقق من وجود subtitles سريعاً
	// cmdListSubs := exec.Command("yt-dlp", "--list-subs", videoURL)
	// subsOutput, _ := cmdListSubs.CombinedOutput()
	// if len(subsOutput) > 0 {
	// 	log.Printf("Subtitles found:\n%s", string(subsOutput))

	// 	// محاولة تنزيل subtitle مباشرة
	// 	lang := "en" // default lang
	// 	cmdSubs := exec.Command("yt-dlp",
	// 		"--skip-download",
	// 		"--write-subs",
	// 		"--write-auto-subs",
	// 		"--sub-langs", lang,
	// 		"--sub-format", "srt/best",
	// 		"--convert-subs", "srt",
	// 		"-o", outputTemplate,
	// 		videoURL,
	// 	)
	// 	if err := cmdSubs.Run(); err == nil {
	// 		srtFile := fmt.Sprintf("%s.%s.srt", outputTemplate, lang)
	// 		return srtFile, nil
	// 	}
	// }

	// 2️⃣ لو ما فيش subtitle → تحميل صوت صغير الحجم فقط
	audioFile := "audio.%(ext)s"
	cmdAudio := exec.Command("yt-dlp",
		"-f", "bestaudio",
		"--extract-audio",
		"--audio-format", "mp3",
		"--audio-quality", "32K", // أقل جودة لتقليل الحجم
		"--postprocessor-args", "ffmpeg:-ac 1 -ar 8000", // mono + 8kHz
		"-o", audioFile,
		videoURL,
	)
	log.Println("Downloading compressed audio...")
	output, err := cmdAudio.CombinedOutput()
	if err != nil {
		log.Printf("yt-dlp audio download error: %s", string(output))
		return "", fmt.Errorf("audio download failed: %w", err)
	}

	audioFileName := "audio.mp3"
	log.Println("Audio downloaded:", audioFileName)

	// Chunk the audio to speed up transcription without affecting timestamps
	chunksDir := "chunks"
	_ = os.RemoveAll(chunksDir)
	if err := os.MkdirAll(chunksDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create chunks dir: %w", err)
	}

	chunkDurationSec := 300 // 5 minutes per chunk
	chunkPattern := filepath.Join(chunksDir, "chunk_%03d.mp3")

	segCmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", audioFileName,
		"-ar", "16000",
		"-ac", "1",
		"-b:a", "32k",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", chunkDurationSec),
		"-reset_timestamps", "1",
		"-y", chunkPattern,
	)
	if out, err := segCmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg segment error: %s", string(out))
		return "", fmt.Errorf("failed to segment audio: %w", err)
	}

	chunkFiles, err := filepath.Glob(filepath.Join(chunksDir, "chunk_*.mp3"))
	if err != nil || len(chunkFiles) == 0 {
		return "", fmt.Errorf("no chunks produced: %w", err)
	}
	// ensure stable order
	sort.Strings(chunkFiles)

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not set")
	}
	client := openai.NewClient(apiKey)

	type chunkResult struct {
		index    int
		text     string
		segments []TranscriptSegment
		err      error
	}

	results := make([]chunkResult, len(chunkFiles))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // limit concurrency

	for i, file := range chunkFiles {
		i, file := i, file
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			resp, err := client.CreateTranscription(
				context.Background(),
				openai.AudioRequest{
					Model:    openai.Whisper1,
					FilePath: file,
					Format:   openai.AudioResponseFormatVerboseJSON,
					TimestampGranularities: []openai.TranscriptionTimestampGranularity{
						openai.TranscriptionTimestampGranularitySegment,
					},
				},
			)
			if err != nil {
				results[i] = chunkResult{index: i, err: err}
				return
			}

			// Map to TranscriptSegment and offset timestamps
			offset := float64(i * chunkDurationSec)
			var segs []TranscriptSegment
			for idx, s := range resp.Segments {
				segs = append(segs, TranscriptSegment{
					ID:               idx,
					Start:            s.Start + offset,
					End:              s.End + offset,
					Text:             s.Text,
					Tokens:           nil,
					Temperature:      0,
					AvgLogprob:       0,
					CompressionRatio: 0,
					NoSpeechProb:     0,
				})
			}
			results[i] = chunkResult{index: i, text: resp.Text, segments: segs, err: nil}
		}()
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			return "", fmt.Errorf("chunk %d transcription failed: %w", r.index, r.err)
		}
	}

	// Merge results in order
	sort.Slice(results, func(a, b int) bool { return results[a].index < results[b].index })
	var merged TranscriptResponse
	var mergedTextParts []string
	for _, r := range results {
		merged.Segments = append(merged.Segments, r.segments...)
		if r.text != "" {
			mergedTextParts = append(mergedTextParts, r.text)
		}
	}
	merged.Text = strings.Join(mergedTextParts, " ")
	if len(merged.Segments) > 0 {
		merged.Duration = merged.Segments[len(merged.Segments)-1].End
	}

	// 4️⃣ احفظ النتيجة كاملة (فيها text + segments)
	transcriptFile := "transcript_segments.json"
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal transcription: %w", err)
	}
	if err := os.WriteFile(transcriptFile, data, 0644); err != nil {
		return "", fmt.Errorf("failed to save transcript: %w", err)
	}

	// Cleanup temp files
	_ = os.Remove(audioFileName)
	_ = os.RemoveAll(chunksDir)

	return transcriptFile, nil
}

// countWordsBeforeKeyword counts words before the first occurrence of the keyword
func countWordsBeforeKeyword(text, keyword string) int {
	lowerText := strings.ToLower(text)
	lowerKeyword := strings.ToLower(keyword)

	keywordIndex := strings.Index(lowerText, lowerKeyword)
	if keywordIndex == -1 {
		return 0
	}

	// Count words in the text before the keyword
	textBeforeKeyword := text[:keywordIndex]
	words := strings.Fields(textBeforeKeyword)
	return len(words)
}

func main() {
	// Load environment variables from .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found, using system environment variables")
	} else {
		log.Printf("Successfully loaded .env file")
	}

	app := NewApp()
	r := gin.New()
	r.GET("/", func(ctx *gin.Context) {
		ctx.String(200, "Hello World!")
	})

	r.POST("/api/search", app.searchHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8800"
	}

	log.Printf("Server running on port %s...", port)
	r.RunTLS(":"+port, "cert.pem", "key.pem")
}
