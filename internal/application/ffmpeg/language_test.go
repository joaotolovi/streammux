package ffmpeg

import "testing"

func TestLanguageCodeNormalization(t *testing.T) {
	tests := map[string]string{
		"Portuguese (Brazil)": "por",
		" PORTUGUÊS ":         "por",
		"pt_BR":               "por",
		"ENG":                 "eng",
		"en-US":               "eng",
		"Español":             "spa",
		"fre":                 "fra",
		"German":              "deu",
		"ja-JP":               "jpn",
		"Chinese (Mandarin)":  "zho",
		"":                    "",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := LanguageCode(input); got != want {
				t.Fatalf("LanguageCode(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestAudioTrackIndexByLanguageNormalizesTags(t *testing.T) {
	tracks := []AudioTrack{
		{Index: 0, Language: "es"},
		{Index: 1, Language: "PT-br"},
		{Index: 2},
	}

	if got := AudioTrackIndexByLanguage(tracks, "por"); got != 1 {
		t.Fatalf("Portuguese index = %d, want 1", got)
	}
	if got := AudioTrackIndexByLanguage(tracks, "English"); got != 2 {
		t.Fatalf("English fallback index = %d, want 2", got)
	}
	if got := AudioTrackIndexByLanguage(tracks, "fra"); got != -1 {
		t.Fatalf("French index = %d, want -1", got)
	}
}
