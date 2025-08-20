package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pemistahl/lingua-go"

	"github.com/gin-gonic/gin"
)

// Subtitle model
type SubtitleEntry struct {
	Start float64
	End   float64
	Text  string
}

// Parser
type SubtitleParser struct{}

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

// Main method: get subtitles from YouTube as string and search keyword
func (app *App) SearchKeywordInSubtitles(videoURL, keyword string) (float64, bool, error) {

	// Detect language of the keyword
	detector := lingua.NewLanguageDetectorBuilder().FromAllLanguages().Build()
	lang, ok := detector.DetectLanguageOf(keyword)
	langCode := "en"
	if ok {
		code := strings.ToLower(lang.IsoCode639_1().String())
		if code != "" {
			langCode = code
		}
	}

	// Use a unique output template to avoid file conflicts
	outputTemplate := "temp_subs"
	cmd := exec.Command("yt-dlp",
		"--skip-download",
		"--write-subs",
		"--write-auto-subs",
		"--sub-langs", langCode,
		"--sub-format", "srt",
		"-o", outputTemplate,
		videoURL,
	)
	if err := cmd.Run(); err != nil {
		return 0, false, fmt.Errorf("failed to fetch SRT subtitles for language '%s': %w", langCode, err)
	}

	// The SRT file will be named temp_subs.<langCode>.srt
	srtFile := fmt.Sprintf("%s.%s.srt", outputTemplate, langCode)
	srtContent, err := os.ReadFile(srtFile)
	if err != nil {
		return 0, false, fmt.Errorf("failed to read SRT file: %w", err)
	}
	// Optionally, clean up the SRT file after reading
	defer os.Remove(srtFile)

	fmt.Printf("--- RAW SRT OUTPUT (%s) START ---\n", langCode)
	fmt.Println(string(srtContent))
	fmt.Println("--- RAW SRT OUTPUT END ---")
	subs, err := app.parser.ParseSRTContent(string(srtContent))
	if err != nil {
		return 0, false, fmt.Errorf("failed to parse SRT subtitles: %w", err)
	}

	lowerKeyword := strings.ToLower(keyword)
	for _, sub := range subs {
		if strings.Contains(strings.ToLower(sub.Text), lowerKeyword) {
			return sub.Start, true, nil
		}
	}

	return 0, false, nil
}

// HTTP Handlers
type SearchRequest struct {
	VideoURL string `json:"video_url"`
	Keyword  string `json:"keyword"`
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
	log.Printf("Sin search")
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{Error: "Invalid JSON request"})
		return
	}
	if req.VideoURL == "" || req.Keyword == "" {
		c.JSON(400, ErrorResponse{Error: "video_url and keyword are required"})
		return
	}

	timestamp, found, err := app.SearchKeywordInSubtitles(req.VideoURL, req.Keyword)
	if err != nil {
		c.JSON(500, ErrorResponse{Error: err.Error()})
		return
	}

	resp := SearchResponse{
		Found:  found,
		Time:   secondsToTimeString(timestamp),
		Source: "subtitles",
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

func main() {
	app := NewApp()
	r := gin.Default()

	r.POST("/search", app.searchHandler)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "timestamp": time.Now().Unix()})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8800"
	}

	log.Printf("Server running on port %s...", port)
	r.Run(":" + port)
}
