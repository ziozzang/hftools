package hub

import "testing"

func TestNormalizeRepoIDAndLocalDirectory(t *testing.T) {
	for input, want := range map[string]string{
		"FluidInference/silero-vad-coreml":                         "FluidInference/silero-vad-coreml",
		"https://huggingface.co/FluidInference/silero-vad-coreml":  "FluidInference/silero-vad-coreml",
		"https://huggingface.co/FluidInference/silero-vad-coreml/": "FluidInference/silero-vad-coreml",
		"bert-base-uncased": "bert-base-uncased",
	} {
		got, err := NormalizeRepoID(input)
		if err != nil || got != want {
			t.Errorf("NormalizeRepoID(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if got := LocalDirectoryName("FluidInference/silero-vad-coreml"); got != "FluidInference_silero-vad-coreml" {
		t.Fatalf("LocalDirectoryName = %q", got)
	}
	for _, input := range []string{"https://huggingface.co/a/b/tree/main", "a/b/c", "../model", "https://user@example.com/a/b"} {
		if _, err := NormalizeRepoID(input); err == nil {
			t.Errorf("invalid repository accepted: %q", input)
		}
	}
}
