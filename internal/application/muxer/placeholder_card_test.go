package muxer

import "testing"

func TestNormalizeCardText(t *testing.T) {
	got := normalizeCardText("\ufeffTitulo\r\nNota\uFFFD\r\nStream\t \n")
	if got != "Titulo\nNota\nStream" {
		t.Fatalf("normalizeCardText() = %q", got)
	}
}
