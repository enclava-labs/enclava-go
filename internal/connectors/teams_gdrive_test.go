package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamsFetcherPaginationIDsAndAttachmentMetadata(t *testing.T) {
	t.Setenv("CONNECTOR_TEAMS_TOKEN", "teams-test-token")
	attachmentRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer teams-test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/v1.0/teams/team-1/channels":
			if r.URL.Query().Get("page") == "next" {
				_, _ = fmt.Fprint(w, `{"value":[]}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"value":[{"id":"channel-1"}],"@odata.nextLink":%q}`, "http://"+r.Host+"/v1.0/teams/team-1/channels?page=next")
		case r.URL.Path == "/v1.0/teams/team-1/channels/channel-1/messages":
			if r.URL.Query().Get("page") == "" {
				_, _ = fmt.Fprintf(w, `{"value":[{"id":"message-1","body":{"content":"hello"},"webUrl":"https://teams.example/message/1","lastModifiedDateTime":"2026-10-20T00:00:00Z","attachments":[{"name":"report.pdf","contentType":"application/pdf","contentUrl":"https://files.example/report"}]}],"@odata.nextLink":%q}`, "http://"+r.Host+"/v1.0/teams/team-1/channels/channel-1/messages?page=next")
				return
			}
			_, _ = fmt.Fprint(w, `{"value":[{"id":"message-2","body":{"content":"second"}}]}`)
		case strings.Contains(r.URL.Path, "/replies"):
			if strings.HasSuffix(r.URL.Path, "/messages/message-1/replies") {
				_, _ = fmt.Fprint(w, `{"value":[{"id":"reply-1","body":{"content":"reply"}}]}`)
			} else {
				_, _ = fmt.Fprint(w, `{"value":[]}`)
			}
		case strings.Contains(r.URL.Path, "/attachments"):
			attachmentRequests++
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	svc := &Service{httpClient: &http.Client{Timeout: 3e9, CheckRedirect: refuseConnectorRedirect}}
	source := Source{Type: SourceTypeTeams, CredentialRef: "CONNECTOR_TEAMS_TOKEN", Config: map[string]any{"base_url": server.URL + "/v1.0", "team_id": "team-1"}}
	first, err := svc.fetchTeamsDocuments(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.fetchTeamsDocuments(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("documents = %d, %d", len(first), len(second))
	}
	if first[0].SourceID != "teams:team-1:channel-1:message-1" || first[0].SourceID != second[0].SourceID {
		t.Fatalf("unstable source ID: %q, %q", first[0].SourceID, second[0].SourceID)
	}
	if first[0].SourceURI != "https://teams.example/message/1" || first[0].Revision != "2026-10-20T00:00:00Z" {
		t.Fatalf("missing Teams metadata: %#v", first[0])
	}
	if !strings.Contains(first[0].Content, "Attachment: report.pdf (application/pdf) https://files.example/report") || !strings.Contains(first[0].Content, "Reply: reply") {
		t.Fatalf("unexpected flattened content: %s", first[0].Content)
	}
	if attachmentRequests != 0 {
		t.Fatalf("attachments were downloaded %d times", attachmentRequests)
	}
}

func TestTeamsFetcherRejectsMalformedPayload(t *testing.T) {
	t.Setenv("CONNECTOR_TEAMS_TOKEN", "teams-test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"value":[{"displayName":"missing id"}]}`)
	}))
	defer server.Close()
	svc := &Service{httpClient: server.Client()}
	_, err := svc.fetchTeamsDocuments(context.Background(), Source{Type: SourceTypeTeams, CredentialRef: "CONNECTOR_TEAMS_TOKEN", Config: map[string]any{"base_url": server.URL, "team_id": "team-1"}})
	if err == nil || !strings.Contains(err.Error(), "channel payload is malformed") {
		t.Fatalf("error = %v", err)
	}
}

func TestGoogleDriveFetcherPaginatesAndExportsGoogleDocs(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "drive-test-token")
	listCalls, exports := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer drive-test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/drive/v3/files":
			listCalls++
			if r.URL.Query().Get("q") != "'folder-1' in parents and trashed = false" {
				t.Errorf("query = %q", r.URL.Query().Get("q"))
			}
			if listCalls == 1 {
				_, _ = fmt.Fprint(w, `{"files":[{"id":"doc-1","name":"Plan","mimeType":"application/vnd.google-apps.document","webViewLink":"https://drive.google.com/file/d/doc-1/view","headRevisionId":"rev-7"}],"nextPageToken":"next"}`)
			} else {
				if r.URL.Query().Get("pageToken") != "next" {
					t.Errorf("page token = %q", r.URL.Query().Get("pageToken"))
				}
				_, _ = fmt.Fprint(w, `{"files":[{"id":"file-2","name":"notes.txt","mimeType":"text/plain","headRevisionId":"rev-2"}]}`)
			}
		case r.URL.Path == "/drive/v3/files/doc-1/export":
			exports++
			_, _ = fmt.Fprint(w, "exported Google Doc")
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	svc := &Service{httpClient: server.Client()}
	source := Source{Type: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive/v3", "folder_id": "folder-1"}}
	docs, err := svc.fetchGoogleDriveDocuments(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 || exports != 1 || len(docs) != 2 {
		t.Fatalf("calls=%d exports=%d docs=%d", listCalls, exports, len(docs))
	}
	if docs[0].Content != "exported Google Doc" || docs[0].SourceID != "google_drive:doc-1" || docs[0].Revision != "rev-7" {
		t.Fatalf("unexpected Google Docs document: %#v", docs[0])
	}
	if docs[1].SourceID != "google_drive:file-2" || docs[1].Revision != "rev-2" || !strings.Contains(docs[1].Content, "notes.txt") {
		t.Fatalf("unexpected metadata document: %#v", docs[1])
	}
}

func TestGoogleDriveFetcherRejectsMalformedPayload(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "drive-test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"files":[{"name":"missing id"}]}`)
	}))
	defer server.Close()
	svc := &Service{httpClient: server.Client()}
	_, err := svc.fetchGoogleDriveDocuments(context.Background(), Source{Type: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL, "folder_id": "folder-1"}})
	if err == nil || !strings.Contains(err.Error(), "file payload is malformed") {
		t.Fatalf("error = %v", err)
	}
}
