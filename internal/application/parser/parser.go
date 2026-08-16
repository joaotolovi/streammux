// Package parser extracts resolution, quality, encode, visual/audio tags,
// channels and languages from stream names/descriptions. The regexes and
// language table mirror the AIOStreams parser for consistent results.
package parser

import (
	"strings"

	"github.com/dlclark/regexp2"

	"github.com/streammux/streammux/internal/domain/model"
)

// createRegex builds a word-boundary aware regex from a pattern. It uses
// regexp2 (ECMAScript) so lookbehind/lookahead are supported, matching the
// AIOStreams parser. The pattern is wrapped in a non-capturing group so the
// boundaries apply to every alternative.
func createRegex(pattern string) *regexp2.Regexp {
	re, err := regexp2.Compile(`(?i)(?<![^\s\[(_\-,.])(?:`+pattern+`)(?=[\s)\]_.,-]|$)`, regexp2.ECMAScript)
	if err != nil {
		return regexp2.MustCompile(pattern, regexp2.ECMAScript|regexp2.IgnoreCase)
	}
	return re
}

// createLanguageRegex avoids matching "sub"/"subtitle" suffixes.
func createLanguageRegex(pattern string) *regexp2.Regexp {
	return createRegex(`(?:` + pattern + `)` + `(?![ .\-_]?sub(title)?s?)`)
}

// matches reports whether the regexp2 pattern matches the input.
func matches(re *regexp2.Regexp, s string) bool {
	ok, _ := re.MatchString(s)
	return ok
}

var (
	resolutionRegexes = map[string]*regexp2.Regexp{
		"2160p": createRegex(`(bd|hd|m)?(4k|2160(p|i)?)|u(ltra)?[ .\-_]?hd|3840\s?x\s?(\d{4})`),
		"1440p": createRegex(`(bd|hd|m)?(1440(p|i)?)|2k|w?q(uad)?[ .\-_]?hd|2560\s?x(\d{4})`),
		"1080p": createRegex(`(bd|hd|m)?(1080(p|i)?)|f(ull)?[ .\-_]?hd|1920\s?x\s?(\d{3,4})`),
		"720p":  createRegex(`(bd|hd|m)?((720|800)(p|i)?)|hd|1280\s?x\s?(\d{3,4})`),
		"576p":  createRegex(`(bd|hd|m)?((576|534)(p|i)?)`),
		"480p":  createRegex(`(bd|hd|m)?(480(p|i)?)|sd`),
		"360p":  createRegex(`(bd|hd|m)?(360(p|i)?)`),
		"240p":  createRegex(`(bd|hd|m)?(240(p|i)?)`),
		"144p":  createRegex(`(bd|hd|m)?(144(p|i)?)`),
	}

	qualityRegexes = map[string]*regexp2.Regexp{
		"BluRay REMUX": createRegex(`(?<!dvd.*)(bd|br|b|uhd)?remux(?!.*dvd)`),
		"BluRay":       createRegex(`(?<!remux.*)((bd|blu[ .\-_]?ray)([ .\-_]?rip)?|br[ .\-_]?rip)(?!.*remux)`),
		"WEB-DL":       createRegex(`web[ .\-_]?(dl)?(?![ .\-_]?(rip|dlrip|cam))`),
		"WEBRip":       createRegex(`web[ .\-_]?rip`),
		"HDRip":        createRegex(`hd[ .\-_]?rip|web[ .\-_]?dl[ .\-_]?rip`),
		"HC HD-Rip":    createRegex(`hc|hd[ .\-_]?rip`),
		"DVD REMUX":    createRegex(`(hd[ .\-_]?)?dvd.*remux|remux.*(hd[ .\-_]?)?dvd`),
		"DVDRip":       createRegex(`dvd[ .\-_]?(rip|mux|r|full|5|9)?`),
		"HDTV":         createRegex(`(hd|pd)tv|tv[ .\-_]?rip|hdtv[ .\-_]?rip|dsr(ip)?|sat[ .\-_]?rip`),
		"CAM":          createRegex(`cam|hdcam|cam[ .\-_]?rip`),
		"TS":           createRegex(`telesync|ts|hd[ .\-_]?ts|pdvd|predvd(rip)?`),
		"TC":           createRegex(`telecine|tc|hd[ .\-_]?tc`),
		"SCR":          createRegex(`((dvd|bd|web|hd)?[ .\-_]?)?(scr(eener)?)`),
	}

	visualTagRegexes = map[string]*regexp2.Regexp{
		"10bit":    createRegex(`10[ .\-_]?bit|hi10p?`),
		"HDR10+":   createRegex(`hdr[ .\-_]?10[ .\-_]?(p(lus)?|[\+])`),
		"HDR10":    createRegex(`hdr[ .\-_]?10(?![ .\-_]?(?:\+|p(?:lus)?|bit|hi))`),
		"HDR":      createRegex(`hdr(?![ .\-_]?(?:10(?![ .\-_]?(?:bit|hi))|\+|p(?:lus)?))`),
		"HLG":      createRegex(`hlg`),
		"DV":       createRegex(`do?(lby)?[ .\-_]?vi?(sion)?(?:[ .\-_]?atmos)?|dv`),
		"3D":       createRegex(`(bd)?(3|three)[ .\-_]?(d(imension)?(al)?)`),
		"IMAX":     createRegex(`imax`),
		"AI":       createRegex(`ai|ai(enhanced?|re[ .\-_]?graded?)`),
		"Upscaled": createRegex(`(ai)?(uprez|ups(uhd|cal(ed?|ing)(uhd)?))(ai)?`),
		"SDR":      createRegex(`sdr`),
		"H-OU":     createRegex(`h?(alf)?[ .\-_]?(ou|over[ .\-_]?under)`),
		"H-SBS":    createRegex(`h?(alf)?[ .\-_]?(sbs|side[ .\-_]?by[ .\-_]?side)`),
	}

	audioTagRegexes = map[string]*regexp2.Regexp{
		"Atmos":     createRegex(`atmos|ddpa\d?`),
		"DD+":       createRegex(`(d(olby)?[ .\-_]?d(igital)?[ .\-_]?((p(lus)?|\+)a?)(?:[ .\-_]?(2[ .\-_]?0|5[ .\-_]?1|7[ .\-_]?1))?)|e[ .\-_]?ac[ .\-_]?3`),
		"DD":        createRegex(`(d(olby)?[ .\-_]?d(igital)?(?:[ .\-_]?(5[ .\-_]?1|7[ .\-_]?1|2[ .\-_]?0?))?)|(?<!e[ .\-_]?)ac[ .\-_]?3`),
		"DTS:X":     createRegex(`dts[ .\-:_]?x`),
		"DTS-HD MA": createRegex(`dts[ .\-_]?hd[ .\-_]?ma`),
		"DTS-HD":    createRegex(`dts[ .\-_]?hd(?![ .\-_]?ma)`),
		"DTS-ES":    createRegex(`dts[ .\-_]?es`),
		"DTS":       createRegex(`dts(?![ .\-:_]?(x(?=[\s)\]_.,-]|$)|hd[ .\-_]?(ma)?|es))`),
		"TrueHD":    createRegex(`true[ .\-_]?hd`),
		"OPUS":      createRegex(`opus`),
		"AAC":       createRegex(`q?aac(?:[ .\-_]?2)?`),
		"FLAC":      createRegex(`flac(?:[ .\-_]?(lossless|2\.0|x[2-4]))?`),
	}

	audioChannelRegexes = map[string]*regexp2.Regexp{
		"2.0": createRegex(`(d(olby)?[ .\-_]?d(igital)?)?2[ .\-_]?0(ch)?`),
		"5.1": createRegex(`(d(olby)?[ .\-_]?d(igital)?[ .\-_]?((p(lus)?|\+)a?)?)?5[ .\-_]?1(ch)?`),
		"6.1": createRegex(`(d(olby)?[ .\-_]?d(igital)?[ .\-_]?((p(lus)?|\+)a?)?)?6[ .\-_]?1(ch)?`),
		"7.1": createRegex(`(d(olby)?[ .\-_]?d(igital)?[ .\-_]?((p(lus)?|\+)a?)?)?7[ .\-_]?1(ch)?`),
	}

	encodeRegexes = map[string]*regexp2.Regexp{
		"AV1":  createRegex(`av1`),
		"HEVC": createRegex(`hevc[ .\-_]?(10)?|[xh][ .\-_]?265`),
		"AVC":  createRegex(`avc|[xh][ .\-_]?264`),
		"VC-1": createRegex(`vc[ .\-_]?1`),
		"XviD": createRegex(`xvid`),
		"DivX": createRegex(`divx|dvix`),
	}

	editionRegexes = map[string]*regexp2.Regexp{
		"Director's Cut":  createRegex(`director'?s?[ .\-_]?cut`),
		"Extended":        createRegex(`extended([ .\-_]?(cut|edition))?`),
		"Theatrical":      createRegex(`theatrical([ .\-_]?(cut|edition))?`),
		"Unrated":         createRegex(`unrated`),
		"Uncut":           createRegex(`uncut`),
		"Final Cut":       createRegex(`final[ .\-_]?cut`),
		"Redux":           createRegex(`redux`),
		"Special Edition": createRegex(`special[ .\-_]?edition`),
	}

	languageRegexes = map[string]*regexp2.Regexp{
		"Multi":               createLanguageRegex(`multi`),
		"Dual Audio":          createLanguageRegex(`dual[ .\-_]?(audio|lang(uage)?|flac|ac3|aac2?)?`),
		"Dubbed":              createLanguageRegex(`dub(s|bed|bing)?|dublad[oa]s?`),
		"English":             createLanguageRegex(`english|eng`),
		"Japanese":            createLanguageRegex(`japanese|jap|jpn`),
		"Chinese":             createLanguageRegex(`chinese|chi`),
		"Russian":             createLanguageRegex(`russian|rus`),
		"Arabic":              createLanguageRegex(`arabic|ara`),
		"Portuguese":          createLanguageRegex(`portugues[a]?|portugu[eê]s`),
		"Portuguese (Brazil)": createLanguageRegex(`portuguese[ .\-_]?brazil|portugues[ .\-_]?brasil|pt[ .\-_]?br`),
		"Spanish":             createLanguageRegex(`spanish|spa|esp`),
		"French":              createLanguageRegex(`french|fra|fr|vf|vff|vfi|vf2|vfq|truefrench`),
		"German":              createLanguageRegex(`deu(tsch)?(land)?|ger(man)?`),
		"Italian":             createLanguageRegex(`italian|ita`),
		"Korean":              createLanguageRegex(`korean|kor`),
		"Hindi":               createLanguageRegex(`hindi|hin`),
		"Bengali":             createLanguageRegex(`bengali|ben(?![ .\-_]?the[ .\-_]?men)`),
		"Punjabi":             createLanguageRegex(`punjabi|pan`),
		"Marathi":             createLanguageRegex(`marathi|mar`),
		"Gujarati":            createLanguageRegex(`gujarati|guj`),
		"Tamil":               createLanguageRegex(`tamil|tam`),
		"Telugu":              createLanguageRegex(`telugu|tel`),
		"Kannada":             createLanguageRegex(`kannada|kan`),
		"Malayalam":           createLanguageRegex(`malayalam|mal`),
		"Thai":                createLanguageRegex(`thai|tha`),
		"Vietnamese":          createLanguageRegex(`vietnamese|vie`),
		"Indonesian":          createLanguageRegex(`indonesian|ind`),
		"Turkish":             createLanguageRegex(`turkish|tur`),
		"Hebrew":              createLanguageRegex(`hebrew|heb`),
		"Persian":             createLanguageRegex(`persian|per`),
		"Ukrainian":           createLanguageRegex(`ukrainian|ukr`),
		"Greek":               createLanguageRegex(`greek|ell`),
		"Lithuanian":          createLanguageRegex(`lithuanian|lit`),
		"Latvian":             createLanguageRegex(`latvian|lav`),
		"Estonian":            createLanguageRegex(`estonian|est`),
		"Polish":              createLanguageRegex(`polish|pol`),
		"Czech":               createLanguageRegex(`czech|cze`),
		"Slovak":              createLanguageRegex(`slovak|slo`),
		"Hungarian":           createLanguageRegex(`hungarian|hun`),
		"Romanian":            createLanguageRegex(`romanian|rum`),
		"Bulgarian":           createLanguageRegex(`bulgarian|bul`),
		"Serbian":             createLanguageRegex(`serbian|srp`),
		"Croatian":            createLanguageRegex(`croatian|hrv`),
		"Slovenian":           createLanguageRegex(`slovenian|slv`),
		"Dutch":               createLanguageRegex(`dutch|dut`),
		"Danish":              createLanguageRegex(`danish|dan`),
		"Finnish":             createLanguageRegex(`finnish|fin`),
		"Swedish":             createLanguageRegex(`swedish|swe`),
		"Norwegian":           createLanguageRegex(`norwegian|nor`),
		"Malay":               createLanguageRegex(`malay`),
		"Latino":              createLanguageRegex(`latino|lat`),
	}

	// flagLanguages maps flag emojis to their canonical language.
	flagLanguages = map[string]string{
		"🇵🇹": "Portuguese (Brazil)",
		"🇧🇷": "Portuguese (Brazil)",
		"🇬🇧": "English",
		"🇺🇸": "English",
		"🇪🇸": "Spanish",
		"🇫🇷": "French",
		"🇩🇪": "German",
		"🇮🇹": "Italian",
		"🇯🇵": "Japanese",
		"🇰🇷": "Korean",
		"🇮🇳": "Hindi",
		"🇷🇺": "Russian",
		"🇨🇳": "Chinese",
		"🇸🇦": "Arabic",
		"🇹🇭": "Thai",
		"🇻🇳": "Vietnamese",
		"🇹🇷": "Turkish",
		"🇵🇱": "Polish",
		"🇺🇦": "Ukrainian",
		"🇬🇷": "Greek",
		"🇳🇱": "Dutch",
		"🇸🇪": "Swedish",
		"🇩🇰": "Danish",
		"🇫🇮": "Finnish",
		"🇳🇴": "Norwegian",
	}

	// codeToLanguage maps ISO 639-1 / 639-2 language codes to display names.
	codeToLanguage = map[string]string{
		"por": "Portuguese (Brazil)", "pt": "Portuguese (Brazil)", "pt-br": "Portuguese (Brazil)", "ptbr": "Portuguese (Brazil)",
		"eng": "English", "en": "English",
		"spa": "Spanish", "es": "Spanish", "esp": "Spanish",
		"fra": "French", "fr": "French", "fre": "French",
		"deu": "German", "de": "German", "ger": "German",
		"ita": "Italian", "it": "Italian",
		"jpn": "Japanese", "ja": "Japanese", "jap": "Japanese",
		"kor": "Korean", "ko": "Korean",
		"hin": "Hindi", "hi": "Hindi",
		"rus": "Russian", "ru": "Russian",
		"zho": "Chinese", "zh": "Chinese", "chi": "Chinese",
		"ara": "Arabic", "ar": "Arabic",
		"tha": "Thai", "th": "Thai",
		"vie": "Vietnamese", "vi": "Vietnamese",
		"tur": "Turkish", "tr": "Turkish",
		"pol": "Polish", "pl": "Polish",
		"ukr": "Ukrainian", "uk": "Ukrainian",
		"ell": "Greek", "el": "Greek", "gre": "Greek",
		"nld": "Dutch", "nl": "Dutch", "dut": "Dutch",
		"swe": "Swedish", "sv": "Swedish",
		"dan": "Danish", "da": "Danish",
		"fin": "Finnish", "fi": "Finnish",
		"nor": "Norwegian", "no": "Norwegian",
		"ces": "Czech", "cs": "Czech", "cze": "Czech",
		"hun": "Hungarian", "hu": "Hungarian",
		"ron": "Romanian", "ro": "Romanian", "rum": "Romanian",
	}

	// releaseGroupRe extracts a trailing release group.
	releaseGroupRe = regexp2.MustCompile(`[-_]([a-zA-Z0-9]+)$`, regexp2.ECMAScript)

	// channelDotRe restores "7.1"/"5.1" markers after separator normalisation.
	channelDotRe = regexp2.MustCompile(`([0-9])·([0-9])`, regexp2.ECMAScript)
)

// Priority orders for single-valued attributes (deterministic matching).
var (
	resolutionOrder = []string{"2160p", "1440p", "1080p", "720p", "576p", "480p", "360p", "240p", "144p"}
	qualityOrder    = []string{"BluRay REMUX", "BluRay", "WEB-DL", "WEBRip", "HDRip", "HC HD-Rip", "DVD REMUX", "DVDRip", "HDTV", "CAM", "TS", "TC", "SCR"}
	encodeOrder     = []string{"AV1", "HEVC", "AVC", "VC-1", "XviD", "DivX"}
	editionOrder    = []string{"Director's Cut", "Extended", "Theatrical", "Unrated", "Uncut", "Final Cut", "Redux", "Special Edition"}
)

func Parse(input string) model.ParsedFile {
	p := model.ParsedFile{}
	if input == "" {
		return p
	}

	// Normalise separators to spaces, preserving "7.1"/"5.1" style markers.
	s := strings.ReplaceAll(input, ".", "·")
	s = strings.ReplaceAll(s, "_", " ")
	s, _ = channelDotRe.Replace(s, "$1.$2", -1, -1)
	s = strings.ReplaceAll(s, "·", " ")

	p.Resolution = matchOrdered(resolutionOrder, resolutionRegexes, s, "Unknown")
	p.Quality = matchOrdered(qualityOrder, qualityRegexes, s, "Unknown")
	p.Encode = matchOrdered(encodeOrder, encodeRegexes, s, "Unknown")
	p.Edition = matchOrdered(editionOrder, editionRegexes, s, "")

	p.VisualTags = matchAllMap(visualTagRegexes, s)
	p.AudioTags = matchAllMap(audioTagRegexes, s)
	p.AudioChannels = matchAllMap(audioChannelRegexes, s)
	p.Languages = detectLanguages(s)

	if m, err := releaseGroupRe.FindStringMatch(s); err == nil && m != nil {
		if g := m.Groups(); len(g) > 1 && len(g[1].Captures) > 0 {
			p.ReleaseGroup = g[1].String()
		}
	}

	return p
}

// matchMap returns the value of the first regex matching the input.
func matchMap(regexes map[string]*regexp2.Regexp, s, fallback string) string {
	for value, re := range regexes {
		if matches(re, s) {
			return value
		}
	}
	return fallback
}

// matchOrdered returns the value of the first regex (in the given priority
// order) matching the input. It is deterministic, unlike iterating a map.
func matchOrdered(order []string, regexes map[string]*regexp2.Regexp, s, fallback string) string {
	for _, value := range order {
		if re, ok := regexes[value]; ok && matches(re, s) {
			return value
		}
	}
	return fallback
}

// matchAllMap returns every value whose regex matches the input.
func matchAllMap(regexes map[string]*regexp2.Regexp, s string) []string {
	var out []string
	for value, re := range regexes {
		if matches(re, s) && !containsStr(out, value) {
			out = append(out, value)
		}
	}
	return out
}

// detectLanguages detects languages via language regexes, flags, and ISO codes.
func detectLanguages(s string) []string {
	var out []string

	for lang, re := range languageRegexes {
		if matches(re, s) && !containsStr(out, lang) {
			out = append(out, lang)
		}
	}

	for flag, lang := range flagLanguages {
		if strings.Contains(s, flag) && !containsStr(out, lang) {
			out = append(out, lang)
		}
	}

	// ISO language codes (pt, pt-br, por, eng, …) as a final signal.
	for code, lang := range codeToLanguage {
		if codeRegexMatch(s, code) && !containsStr(out, lang) {
			out = append(out, lang)
		}
	}

	return out
}

// codeRegexMatch matches an ISO code as a standalone token (e.g. "pt", "pt-br").
func codeRegexMatch(s, code string) bool {
	pattern := regexp2.Escape(code)
	re := regexp2.MustCompile(`(?i)(?<![a-z0-9])`+pattern+`(?![a-z0-9])`, regexp2.ECMAScript)
	ok, _ := re.MatchString(s)
	return ok
}

// DetectLanguage returns the primary language of a stream. Priority is
// deterministic: dubbed (Portuguese) first, then a fixed ordering so the
// result does not depend on map iteration order.
func DetectLanguage(input string) string {
	p := Parse(input)

	// Portuguese variants and Dual Audio take priority (StreamMux's core use
	// case is matching dubbed Portuguese content).
	for _, lang := range p.Languages {
		if lang == "Portuguese" || lang == "Portuguese (Brazil)" || lang == "Dual Audio" || lang == "Dubbed" {
			return "Portuguese (Brazil)"
		}
	}

	// Fixed priority order for the remaining languages.
	priority := []string{
		"English", "Spanish", "French", "German", "Italian", "Japanese",
		"Korean", "Hindi", "Chinese", "Russian", "Arabic", "Turkish", "Polish",
		"Dutch", "Swedish", "Danish", "Finnish", "Norwegian", "Greek",
		"Ukrainian", "Czech", "Hungarian", "Romanian", "Thai", "Vietnamese",
		"Indonesian", "Hebrew", "Persian", "Latino", "Multi",
	}
	for _, want := range priority {
		if containsStr(p.Languages, want) {
			return want
		}
	}

	for _, lang := range p.Languages {
		if lang != "" && lang != "Subbed" {
			return lang
		}
	}
	return "English"
}

// IsDubbed reports whether the stream contains an audio track in the target
// language.
func IsDubbed(input, targetLanguage string) bool {
	p := Parse(input)
	for _, lang := range p.Languages {
		if lang == targetLanguage || lang == "Dual Audio" || lang == "Dubbed" {
			return true
		}
		// Portuguese (pt) also matches the Brazilian target.
		if lang == "Portuguese" && targetLanguage == "Portuguese (Brazil)" {
			return true
		}
		if lang == "Portuguese (Brazil)" && targetLanguage == "Portuguese" {
			return true
		}
	}
	return false
}

// LanguageCode returns the ISO 639-2 code for a display name.
func LanguageCode(lang string) string {
	switch lang {
	case "Portuguese (Brazil)", "Portuguese":
		return "por"
	case "English":
		return "eng"
	case "Spanish":
		return "spa"
	case "French":
		return "fra"
	case "German":
		return "deu"
	case "Italian":
		return "ita"
	case "Japanese":
		return "jpn"
	case "Korean":
		return "kor"
	case "Hindi":
		return "hin"
	case "Russian":
		return "rus"
	case "Chinese":
		return "chi"
	case "Arabic":
		return "ara"
	}
	return "eng"
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
