package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/source"
)

func TestGDriveAdapterPagesAndExportsGoogleDoc(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	listPages, exports := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/drive/files":
			listPages++
			if listPages == 1 {
				fmt.Fprint(w, `{"nextPageToken":"next","files":[{"id":"doc","name":"Guide","mimeType":"application/vnd.google-apps.document","webViewLink":"https://drive.google.com/open?id=doc","headRevisionId":"rev1","parents":["root"]}]}`)
			} else {
				fmt.Fprint(w, `{"files":[{"id":"pdf","name":"manual.pdf","mimeType":"application/pdf","webViewLink":"https://drive.google.com/open?id=pdf","headRevisionId":"rev2"}]}`)
			}
		case "/drive/files/doc/export":
			exports++
			if r.URL.Query().Get("mimeType") != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
				t.Errorf("export mimeType = %q", r.URL.Query().Get("mimeType"))
			}
			_, _ = w.Write([]byte("docx bytes"))
		case "/drive/files/pdf":
			_, _ = w.Write([]byte("pdf bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive", "folder_id": "root"}}
	a := NewGDriveAdapter(&Service{httpClient: server.Client()})
	p1, err := a.Content(context.Background(), c, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 1 || p1.Items[0].NativeRevision != "rev1" || p1.Items[0].URL != "https://drive.google.com/open?id=doc" || string(p1.Items[0].Content) != "docx bytes" || p1.Items[0].MIMEType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || p1.NextCursor != "next" {
		t.Fatalf("page 1 = %#v", p1)
	}
	p2, err := a.Content(context.Background(), c, p1.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 1 || string(p2.Items[0].Content) != "pdf bytes" || p2.Items[0].MIMEType != "application/pdf" || p2.NextCursor != "" {
		t.Fatalf("page 2 = %#v", p2)
	}
	if listPages != 2 || exports != 1 {
		t.Fatalf("calls list=%d export=%d", listPages, exports)
	}
}

func TestGDriveAdapterReadsEverySheetAndPermissionsFailClosed(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	ranges := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/drive/files":
			if r.URL.Query().Get("fields") != "nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime,headRevisionId,parents,shortcutDetails,exportLinks,size)" { // Access requests a separate permission field projection.
				fmt.Fprint(w, `{"files":[{"id":"book","permissions":[{"id":"user:1","type":"user","emailAddress":"a@example.test","role":"reader","permissionDetails":[{"inherited":true,"role":"reader"}]}]}]}`)
				return
			}
			fmt.Fprint(w, `{"files":[{"id":"book","name":"Book","mimeType":"application/vnd.google-apps.spreadsheet","webViewLink":"https://drive.google.com/open?id=book","headRevisionId":"rev"}]}`)
		case r.URL.Path == "/sheets/spreadsheets/book":
			fmt.Fprint(w, `{"sheets":[{"properties":{"sheetId":7,"title":"North","gridProperties":{"rowCount":2,"columnCount":2}}},{"properties":{"sheetId":8,"title":"South","gridProperties":{"rowCount":1,"columnCount":1}}}]}`)
		case strings.HasSuffix(r.URL.Path, "/'North'!A1:B2"):
			ranges["North"] = true
			fmt.Fprint(w, `{"values":[["N1","N2"],["N3","N4"]]}`)
		case strings.HasSuffix(r.URL.Path, "/'South'!A1:A1"):
			ranges["South"] = true
			fmt.Fprint(w, `{"values":[["S1"]]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive", "sheets_base_url": server.URL + "/sheets", "folder_id": "root"}}
	a := NewGDriveAdapter(&Service{httpClient: server.Client()})
	p, err := a.Content(context.Background(), c, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var workbook struct {
		Sheets []struct {
			SheetID int64   `json:"sheet_id"`
			Title   string  `json:"title"`
			Values  [][]any `json:"values"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(p.Items[0].Content, &workbook); err != nil {
		t.Fatal(err)
	}
	if len(workbook.Sheets) != 2 || workbook.Sheets[0].SheetID != 7 || workbook.Sheets[1].Title != "South" || workbook.Sheets[0].Values[1][1] != "N4" || !ranges["North"] || !ranges["South"] {
		t.Fatalf("workbook = %#v ranges=%v", workbook, ranges)
	}
	access, err := a.Access(context.Background(), c, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if access.Completeness != CompletenessComplete || len(access.Grants) != 1 || !access.Grants[0].Inherited || access.Grants[0].NativePrincipalID != "a@example.test" {
		t.Fatalf("access = %#v", access)
	}
}
