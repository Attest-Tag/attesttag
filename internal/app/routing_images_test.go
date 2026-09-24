package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

// A thread with a screenshot in it escalated to the text-only heavy model used to fail the whole
// turn with OpenRouter's 404 "No endpoints found that support image input". The turn keeps its
// model: images go to one the catalogue says takes them, a text-only one gets their file names
// instead, and one the catalogue does not describe is given them.
func TestImagesFor(t *testing.T) {
	llm := NewLLM(Config{})
	llm.models = []ModelInfo{
		{ID: "z-ai/glm-5.3", Inputs: []string{"text"}},
		{ID: "z-ai/glm-5.3-flash", Inputs: []string{"text", "image", "video"}},
	}
	llm.modelsAt = time.Now() // the catalogue is fresh: no provider call
	ctx := context.Background()
	img := openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
		openai.TextContentPart("alex (<@U1>): look\n[attached image: shot.png]"),
		openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: "data:image/png;base64,AAAA"}),
	})
	msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("sys"), img}
	parts := func(out []openai.ChatCompletionMessageParamUnion) int {
		return len(out[1].OfUser.Content.OfArrayOfContentParts)
	}

	if why, out := imagesFor(ctx, llm, "z-ai/glm-5.3-flash", "default", msgs); why != "default" || parts(out) != 2 {
		t.Errorf("model takes images: %s parts=%d", why, parts(out))
	}
	why, out := imagesFor(ctx, llm, "z-ai/glm-5.3", "long thread", msgs)
	u := out[1].OfUser
	if !strings.Contains(why, "images sent by name") || u == nil || parts(out) != 0 ||
		!strings.Contains(u.Content.OfString.Value, "shot.png") || !strings.Contains(u.Content.OfString.Value, "could not be shown") {
		t.Errorf("text-only model: %s %+v; want the file name and no image", why, u)
	}
	if out[0].OfSystem == nil || len(msgs[1].OfUser.Content.OfArrayOfContentParts) != 2 {
		t.Error("dropping images must not touch other messages or the caller's slice")
	}
	if _, out := imagesFor(ctx, llm, "other/x", "default", msgs); parts(out) != 2 {
		t.Errorf("unknown model must be left alone: parts=%d", parts(out))
	}
	if ok, known := llm.acceptsImages(ctx, "z-ai/glm-5.3:nitro"); ok || !known {
		t.Errorf("variant suffix: accepts=%v known=%v, want the base model's answer", ok, known)
	}
}
