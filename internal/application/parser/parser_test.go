package parser

import "testing"

func TestParseLanguageVariants(t *testing.T) {
	cases := []struct {
		input    string
		wantLang []string
	}{
		{"The.Matrix.1999.2160p.BluRay.REMUX.DUAL.Audio.mkv", []string{"Dual Audio"}},
		{"Filme.2019.1080p.WEB-DL.Dublado.mkv", []string{"Dubbed"}},
		{"Movie.2020.2160p.BluRay.pt-BR.mkv", []string{"Portuguese (Brazil)"}},
		{"Movie.2020.2160p.BluRay.PT-BR.DTS.mkv", []string{"Portuguese (Brazil)"}},
		{"Movie.2020.1080p.portugues.mkv", []string{"Portuguese"}},
		{"Movie.2020.1080p.POR.mkv", []string{"Portuguese (Brazil)"}},
		{"Movie.2020.2160p.REMUX.DUAL.ENG.POR.mkv", []string{"Dual Audio", "English", "Portuguese (Brazil)"}},
		{"Movie.2020.1080p.spanish.latino.mkv", []string{"Spanish", "Latino"}},
		{"Movie.2020.1080p.VFF.TrueFrench.mkv", []string{"French"}},
		{"Movie.2020.1080p.DUAL.AUDIO.5.1.mkv", []string{"Dual Audio"}},
	}

	for _, c := range cases {
		p := Parse(c.input)
		if !containsAll(p.Languages, c.wantLang) {
			t.Errorf("input %q: got languages %v, want subset %v", c.input, p.Languages, c.wantLang)
		}
	}
}

func TestParseFlags(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"🎬 Filme 🗣️ 🇧🇷 / 🇺🇸", "Portuguese (Brazil)"},
		{"🎬 Filme 🗣️ 🇵🇹", "Portuguese (Brazil)"},
		{"🎬 Filme 🗣️ 🇬🇧 / 🇪🇸", "English"},
	}
	for _, c := range cases {
		got := DetectLanguage(c.input)
		if got != c.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestParseAudioVideo(t *testing.T) {
	p := Parse("The.Matrix.1999.PROPER.2160p.BluRay.REMUX.HDR.DoVi.HEVC.DTS-HD.MA.TrueHD.7.1.Atmos-FGT.mkv")
	if p.Resolution != "2160p" {
		t.Errorf("resolution = %q, want 2160p", p.Resolution)
	}
	if p.Quality != "BluRay REMUX" {
		t.Errorf("quality = %q, want BluRay REMUX", p.Quality)
	}
	if p.Encode != "HEVC" {
		t.Errorf("encode = %q, want HEVC", p.Encode)
	}
	if !containsAll(p.VisualTags, []string{"HDR", "DV"}) {
		t.Errorf("visual tags = %v, want HDR+DV", p.VisualTags)
	}
	if !containsAll(p.AudioTags, []string{"Atmos", "TrueHD", "DTS-HD MA"}) {
		t.Errorf("audio tags = %v, want Atmos+TrueHD+DTS-HD MA", p.AudioTags)
	}
	if !containsAll(p.AudioChannels, []string{"7.1"}) {
		t.Errorf("channels = %v, want 7.1", p.AudioChannels)
	}
}

func TestParseDubbedDetection(t *testing.T) {
	if !IsDubbed("Movie.2020.1080p.Dublado.mkv", "Portuguese (Brazil)") {
		t.Error("Dublado should be detected as dubbed")
	}
	if !IsDubbed("Movie.2020.1080p.pt-br.mkv", "Portuguese (Brazil)") {
		t.Error("pt-br should be detected as dubbed")
	}
	if !IsDubbed("Movie.2020.1080p.POR.mkv", "Portuguese (Brazil)") {
		t.Error("POR should be detected as dubbed")
	}
	if IsDubbed("Movie.2020.1080p.english.mkv", "Portuguese (Brazil)") {
		t.Error("english should NOT be detected as dubbed for Portuguese")
	}
}

func containsAll(haystack, needles []string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
