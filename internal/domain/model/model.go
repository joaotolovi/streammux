package model

import (
	"strconv"

	"github.com/streammux/streammux/internal/domain/constants"
)

type Service struct {
	ID          string            `json:"id"`
	Enabled     bool              `json:"enabled"`
	Credentials map[string]string `json:"credentials"`
}

type Addon struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	ManifestURL           string   `json:"manifestUrl"`
	Role                  string   `json:"role"`
	Language              string   `json:"language"`
	Enabled               bool     `json:"enabled"`
	Timeout               int      `json:"timeout,omitempty"`
	ShowAllAudioLanguages bool     `json:"showAllAudioLanguages,omitempty"`
	AudioLanguages        []string `json:"audioLanguages,omitempty"`
}

type Config struct {
	Language string    `json:"language"`
	Services []Service `json:"services"`
	Addons   []Addon   `json:"addons"`
}

type User struct {
	UUID              string `json:"uuid"`
	EncryptedPassword string `json:"encryptedPassword"`
	Config            Config `json:"config"`
}

type ParsedFile struct {
	Resolution    string   `json:"resolution"`
	Quality       string   `json:"quality"`
	Encode        string   `json:"encode"`
	Edition       string   `json:"edition,omitempty"`
	VisualTags    []string `json:"visualTags"`
	AudioTags     []string `json:"audioTags"`
	AudioChannels []string `json:"audioChannels"`
	Languages     []string `json:"languages"`
	ReleaseGroup  string   `json:"releaseGroup"`
}

type CollectedStream struct {
	AddonID       string     `json:"addonId"`
	AddonName     string     `json:"addonName"`
	AddonRole     string     `json:"addonRole"`
	AddonLanguage string     `json:"addonLanguage"`
	Stream        Stream     `json:"stream"`
	Parsed        ParsedFile `json:"parsed"`
	Size          int64      `json:"size"`
	// VideoBitrate is the peak video bitrate in bits/s when the source
	// description advertises it (e.g. "📊 62.4 Mbps"). Zero when unknown.
	VideoBitrate int64  `json:"videoBitrate,omitempty"`
	IsDubbed     bool   `json:"isDubbed"`
	Language     string `json:"language"`
}

// SourceKey identifies the underlying media without resolving it. It is stable
// across signed debrid URLs when the addon supplies an info hash.
func (s CollectedStream) SourceKey() string {
	if s.Stream.InfoHash != "" {
		index := ""
		if s.Stream.FileIdx != nil {
			index = strconv.Itoa(*s.Stream.FileIdx)
		}
		return "torrent:" + s.Stream.InfoHash + ":" + index
	}
	return "url:" + s.Stream.URL
}

type Stream struct {
	Name          string         `json:"name"`
	Title         string         `json:"title,omitempty"`
	Description   string         `json:"description,omitempty"`
	URL           string         `json:"url"`
	InfoHash      string         `json:"infoHash,omitempty"`
	FileIdx       *int           `json:"fileIdx,omitempty"`
	Size          int64          `json:"size,omitempty"`
	BehaviorHints map[string]any `json:"behaviorHints,omitempty"`
}

type StremioStream struct {
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	URL           string         `json:"url,omitempty"`
	InfoHash      string         `json:"infoHash,omitempty"`
	FileIdx       *int           `json:"fileIdx,omitempty"`
	BehaviorHints map[string]any `json:"behaviorHints,omitempty"`
}

type PlaybackPlanKind string

const (
	PlanDualSource        PlaybackPlanKind = "dual-source"
	PlanSingleSource      PlaybackPlanKind = "single-source"
	PlanSubtitledFallback PlaybackPlanKind = "subtitled-fallback"
)

// PlaybackPlan is an ordered, immutable playback option. Plans are tried in
// order by the startup coordinator. A zero Audio value means the plan uses the
// first/default audio stream from Video, which is reserved for the final
// subtitled fallback.
type PlaybackPlan struct {
	ID             string
	Kind           PlaybackPlanKind
	Video          CollectedStream
	Audio          CollectedStream
	HasTargetAudio bool
	VideoScore     int
	AudioScore     int
}

// EstimatedBandwidth returns a rough peak bitrate (bits/s) for the plan's
// video source, used to advertise ABR variants in the master playlist. It is
// derived from resolution and encode type; the actual bitrate is unknown until
// the source is probed, so this is a conservative upper bound.
func (p PlaybackPlan) EstimatedBandwidth() int {
	// Prefer the real bitrate advertised by the source description (e.g.
	// "📊 62.4 Mbps") — it is far more accurate than a resolution-based guess
	// (two 2160p sources can be 62 Mbps vs 6 Mbps).
	if p.Video.VideoBitrate > 0 {
		return int(p.Video.VideoBitrate)
	}
	res := p.Video.Parsed.Resolution
	encode := p.Video.Parsed.Encode
	base := 0
	switch res {
	case "2160p":
		base = 25_000_000
	case "1440p":
		base = 15_000_000
	case "1080p":
		base = 8_000_000
	case "720p":
		base = 4_000_000
	case "576p":
		base = 2_500_000
	case "480p":
		base = 1_500_000
	default:
		base = 1_000_000
	}
	// HEVC/AV1 deliver the same quality at roughly half the bitrate of AVC.
	switch encode {
	case "HEVC", "AV1":
		base = base * 2 / 3
	}
	return base
}

func (p PlaybackPlan) VideoURL() string { return p.Video.Stream.URL }

func (p PlaybackPlan) AudioURL() string {
	if p.Audio.Stream.URL != "" {
		return p.Audio.Stream.URL
	}
	return p.Video.Stream.URL
}

func (p PlaybackPlan) SingleSource() bool {
	if p.Audio.Stream.URL == "" && p.Audio.Stream.InfoHash == "" {
		return true
	}
	return p.Audio.SourceKey() == p.Video.SourceKey()
}

// TierMeta advertises one ABR variant in the master playlist. Tiers are
// severity signals ("give me something lighter"), not bindings: behind each
// tier the server walks a downgrade ladder of strategies (lighter sources or
// an on-the-fly transcode) picking the highest expected quality.
type TierMeta struct {
	Bandwidth int64 `json:"bandwidth"`
	Width     int   `json:"width,omitempty"`
	Height    int   `json:"height,omitempty"`
}

type MuxJob struct {
	ID             string         `json:"id"`
	TargetLanguage string         `json:"targetLanguage"`
	Title          string         `json:"title"`
	Plans          []PlaybackPlan `json:"-"`
	Config         Config         `json:"-"`

	// Runtime fields are managed by the muxer and never serialized.
	CacheDir      string  `json:"-"`
	Duration      float64 `json:"-"`
	PlaylistReady bool    `json:"-"`

	// ContentType and ContentID identify the Stremio item being requested.
	// They are set during Process so downstream components can fetch metadata
	// (e.g. poster art) while the film is prepared.
	ContentType string `json:"-"`
	ContentID   string `json:"-"`

	// TierMetas describes the ABR variants served in the master playlist,
	// computed from plan metadata at Process time. Index 0 is the primary
	// (best) plan; higher indexes are progressively lighter.
	TierMetas []TierMeta `json:"tierMetas,omitempty"`
}

type Manifest struct {
	ID            string            `json:"id"`
	Version       string            `json:"version"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Types         []string          `json:"types"`
	Resources     []string          `json:"resources"`
	Logo          string            `json:"logo,omitempty"`
	Catalogs      []ManifestCatalog `json:"catalogs,omitempty"`
	BehaviorHints map[string]any    `json:"behaviorHints,omitempty"`
}

type ManifestCatalog struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ExtraTypes []string `json:"extraTypes,omitempty"`
}

func (c *Config) ValidAddons() []Addon {
	var out []Addon
	for _, a := range c.Addons {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (c *Config) ValidServices() []Service {
	var out []Service
	for _, s := range c.Services {
		if s.Enabled && len(s.Credentials) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) AddonsByRole(role string) []Addon {
	var out []Addon
	for _, a := range c.ValidAddons() {
		if a.Role == role || a.Role == constants.RoleBoth {
			out = append(out, a)
		}
	}
	return out
}
