package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/streammux/streammux/internal/application/parser"
	"github.com/streammux/streammux/internal/domain/model"
)

type Collector struct {
	client *http.Client
}

func New() *Collector {
	return &Collector{
		client: &http.Client{Timeout: 0},
	}
}

func (c *Collector) CollectStreams(ctx context.Context, addons []model.Addon, contentType, contentID string) []model.CollectedStream {
	var all []model.CollectedStream
	type result struct {
		addonID       string
		addonName     string
		addonRole     string
		addonLanguage string
		streams       []model.Stream
		err           error
	}

	results := make(chan result, len(addons))
	for _, addon := range addons {
		go func(a model.Addon) {
			streams, err := c.fetchStreams(ctx, a, contentType, contentID)
			results <- result{addonID: a.ID, addonName: a.Name, addonRole: a.Role, addonLanguage: a.Language, streams: streams, err: err}
		}(addon)
	}

	for i := 0; i < len(addons); i++ {
		r := <-results
		if r.err != nil {
			continue
		}
		for _, s := range r.streams {
			// AIOStreams-style addons put the rich info in `description` (with
			// language flags) and the resolution in `name`/`title`. Parse the
			// concatenation of all three so every signal is available.
			parseSource := strings.Join(nonEmpty(s.Name, s.Title, s.Description), " ")
			parsed := parser.Parse(parseSource)
			lang := parser.DetectLanguage(parseSource)
			dubbed := parser.IsDubbed(parseSource, addonLanguageFor(r.addonLanguage, lang))
			size := s.Size
			if size == 0 {
				size = extractSize(parseSource)
			}
			all = append(all, model.CollectedStream{
				AddonID:       r.addonID,
				AddonName:     r.addonName,
				AddonRole:     r.addonRole,
				AddonLanguage: r.addonLanguage,
				Stream:        s,
				Parsed:        parsed,
				Size:          size,
				IsDubbed:      dubbed,
				Language:      lang,
			})
		}
	}

	return all
}

func (c *Collector) fetchStreams(ctx context.Context, addon model.Addon, contentType, contentID string) ([]model.Stream, error) {
	manifestURL := addon.ManifestURL
	if strings.Contains(manifestURL, "/manifest.json") {
		manifestURL = strings.TrimSuffix(manifestURL, "/manifest.json")
	}
	streamURL := fmt.Sprintf("%s/stream/%s/%s.json", manifestURL, contentType, url.PathEscape(contentID))

	// Per-addon timeout, defaulting to 20s (addons can be slow to aggregate).
	timeout := time.Duration(addon.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", streamURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "StreamMux/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("addon returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Streams []model.Stream `json:"streams"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return result.Streams, nil
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// addonLanguageFor resolves the target language used for dubbing detection:
// the addon's configured language wins when set, otherwise fall back to the
// parsed language.
func addonLanguageFor(addonLanguage, parsedLang string) string {
	if addonLanguage != "" {
		return addonLanguage
	}
	return parsedLang
}

var sizeRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*(GB|MB|TB|GiB|MiB|TiB)`)

// extractSize parses a human-readable size (e.g. "📦 191 GB") into bytes.
func extractSize(s string) int64 {
	m := sizeRe.FindStringSubmatch(s)
	if len(m) < 3 {
		return 0
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(m[2]) {
	case "GB", "GIB":
		return int64(val * 1024 * 1024 * 1024)
	case "MB", "MIB":
		return int64(val * 1024 * 1024)
	case "TB", "TIB":
		return int64(val * 1024 * 1024 * 1024 * 1024)
	}
	return 0
}
