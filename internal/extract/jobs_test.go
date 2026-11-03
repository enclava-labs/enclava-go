package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWorkerPayloadUsesSupportedTextAndImageParts(t *testing.T) {
	svc := &Service{}
	base := Template{ID: "task", OutputSchema: map[string]any{"type": "object"}}
	for _, tc := range []struct {
		name, mime, text string
		data             []byte
		image            bool
	}{
		{name: "parsed document text", mime: "application/pdf", text: "invoice evidence"},
		{name: "image recognition", mime: "image/png", text: "describe labels", data: []byte("image-bytes"), image: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(svc.buildProviderRequest("model", base, "system", tc.text, ProcessInput{
				MimeType: tc.mime, Bytes: tc.data, DocumentText: tc.text,
			}, 0))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(payload), "input_file") {
				t.Fatalf("unsupported input_file payload emitted: %s", payload)
			}
			if tc.image != strings.Contains(string(payload), "image_url") {
				t.Fatalf("image payload mismatch: %s", payload)
			}
			if tc.text != "" && !strings.Contains(string(payload), tc.text) {
				t.Fatalf("request omitted task/document text: %s", payload)
			}
		})
	}
}

func TestUploadMetadataRejectsMismatchedAndNonImageVision(t *testing.T) {
	if err := validateUploadMetadata(ProcessInput{FileName: "scan.pdf", MimeType: "image/png"}); err == nil {
		t.Fatal("MIME/extension mismatch was accepted")
	}
	if err := validateUploadMetadata(ProcessInput{Operation: OperationVision, FileName: "scan.pdf", MimeType: "application/pdf"}); err == nil {
		t.Fatal("vision accepted a non-image input")
	}
}
func TestCopyUploadValidatesImageContentAndDimensions(t *testing.T) {
	var dst bytes.Buffer
	if err := copyUpload(context.Background(), &dst, strings.NewReader("not a PNG"), "image.png"); err == nil {
		t.Fatal("malformed PNG accepted")
	}
	imageBytes, err := os.ReadFile("testdata/scanned-page.png")
	if err != nil {
		t.Fatal(err)
	}
	dst.Reset()
	if err := copyUpload(context.Background(), &dst, bytes.NewReader(imageBytes), "image.png"); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), imageBytes) {
		t.Fatal("image stream was altered")
	}
}
