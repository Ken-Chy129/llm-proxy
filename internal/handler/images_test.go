package handler

import "testing"

func TestImageHostModelDefaultsAndOverrides(t *testing.T) {
	if got := NewImagesHandler(nil, nil, "").hostModel; got != defaultImageHostModel {
		t.Fatalf("default host model = %q, want %q", got, defaultImageHostModel)
	}
	h := NewImagesHandler(nil, nil, " gpt-5.4 ")
	if h.hostModel != "gpt-5.4" {
		t.Fatalf("configured host model = %q, want gpt-5.4", h.hostModel)
	}
	if got := buildCodexImageRequest(h.hostModel, &imageGenRequest{Model: "gpt-image-2", Prompt: "cat"}).Model; got != "gpt-5.4" {
		t.Errorf("generation request model = %q, want gpt-5.4", got)
	}
	if got := buildCodexImageEditRequest(h.hostModel, "gpt-image-2", "cat", "data:image/png;base64,AA", "", "").Model; got != "gpt-5.4" {
		t.Errorf("edit request model = %q, want gpt-5.4", got)
	}
}
