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
		case "/drive/changes/startPageToken":
			fmt.Fprint(w, `{"startPageToken":"start-1"}`)
		case "/drive/files":
			listPages++
			if listPages == 1 {
				fmt.Fprint(w, `{"nextPageToken":"next","files":[{"id":"doc","name":"Guide","mimeType":"application/vnd.google-apps.document","webViewLink":"https://drive.google.com/open?id=doc","headRevisionId":"rev1","parents":["root"]}]}`)
			} else {
				fmt.Fprint(w, `{"files":[{"id":"pdf","name":"manual.pdf","mimeType":"application/pdf","webViewLink":"https://drive.google.com/open?id=pdf","headRevisionId":"rev2"},{"id":"shortcut","name":"outside shortcut","mimeType":"application/vnd.google-apps.shortcut","shortcutDetails":{"targetId":"outside-scope"}}]}`)
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
	if len(p1.Items) != 1 || p1.Items[0].NativeRevision != "rev1" || p1.Items[0].URL != "https://drive.google.com/open?id=doc" || string(p1.Items[0].Content) != "docx bytes" || p1.Items[0].MIMEType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || p1.NextCursor == "" {
		t.Fatalf("page 1 = %#v", p1)
	}
	p2, err := a.Content(context.Background(), c, p1.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeDriveCursor(p2.NextCursor)
	if err != nil || state.Mode != "changes" || state.Token != "start-1" {
		t.Fatalf("checkpoint = %#v, error = %v", state, err)
	}
	if len(p2.Items) != 1 || string(p2.Items[0].Content) != "pdf bytes" || p2.Items[0].MIMEType != "application/pdf" {
		t.Fatalf("page 2 = %#v", p2)
	}
	if listPages != 2 || exports != 1 {
		t.Fatalf("calls list=%d export=%d", listPages, exports)
	}
}

func TestGDriveAdapterReadsEverySheetAndRestrictions(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	ranges := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/drive/changes/startPageToken":
			fmt.Fprint(w, `{"startPageToken":"sheet-start"}`)
		case r.URL.Path == "/drive/files":
			if strings.Contains(r.URL.Query().Get("fields"), "permissions(") {
				fmt.Fprint(w, `{"files":[{"id":"book","driveId":"shared-drive-1","inheritedPermissionsDisabled":false,"copyRequiresWriterPermission":true,"downloadRestrictions":{"itemDownloadRestriction":{"restrictedForReaders":true,"restrictedForWriters":false}},"permissions":[{"id":"user:1","type":"user","emailAddress":"a@example.test","role":"reader","permissionDetails":[{"inherited":true,"inheritedFrom":"shared-drive-1","role":"reader"}]}]}]}`)
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
		NativeMIME string `json:"native_mime"`
		ExportMIME string `json:"export_mime"`
		Sheets     []struct {
			SheetID int64   `json:"sheet_id"`
			Title   string  `json:"title"`
			Values  [][]any `json:"values"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(p.Items[0].Content, &workbook); err != nil {
		t.Fatal(err)
	}
	if workbook.NativeMIME != driveGoogleSheet || workbook.ExportMIME != "application/json" || len(workbook.Sheets) != 2 || workbook.Sheets[0].SheetID != 7 || workbook.Sheets[1].Title != "South" || workbook.Sheets[0].Values[1][1] != "N4" || !ranges["North"] || !ranges["South"] {
		t.Fatalf("workbook = %#v ranges=%v", workbook, ranges)
	}
	access, err := a.Access(context.Background(), c, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if access.Completeness != CompletenessComplete || len(access.Grants) != 4 {
		t.Fatalf("access = %#v", access)
	}
	operations := map[string]int{}
	for _, grant := range access.Grants {
		if grant.Effect == "deny" {
			operations[grant.Scope]++
		}
		if grant.Effect == "allow" && (!grant.Inherited || grant.NativePrincipalID != "a@example.test" || !strings.Contains(grant.Scope, "shared drive shared-drive-1")) {
			t.Fatalf("shared-drive grant = %#v", grant)
		}
	}
	if operations["copy"] != 1 || operations["download"] != 2 {
		t.Fatalf("restricted operations = %v", operations)
	}
}

func TestGDriveChangesDeferDeletesUntilAllPagesComplete(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/drive/changes" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("pageToken") == "old" {
			fmt.Fprint(w, `{"nextPageToken":"next","changes":[{"fileId":"known","removed":true}]}`)
		} else {
			fmt.Fprint(w, `{"newStartPageToken":"current","changes":[]}`)
		}
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive", "folder_id": "root"}}
	a := NewGDriveAdapter(&Service{httpClient: server.Client()})
	cursor, _ := encodeDriveCursor(driveCursor{Mode: "changes", Token: "old", InScope: []string{"known"}})
	first, err := a.Content(context.Background(), c, cursor, 10)
	if err != nil || len(first.Items) != 0 || first.Completeness != CompletenessComplete {
		t.Fatalf("partial delta = %#v, error = %v", first, err)
	}
	second, err := a.Content(context.Background(), c, first.NextCursor, 10)
	if err != nil || len(second.Items) != 0 {
		t.Fatalf("completed delta page = %#v, error = %v", second, err)
	}
	third, err := a.Content(context.Background(), c, second.NextCursor, 10)
	if err != nil || len(third.Items) != 1 || third.Items[0].NativeID != "known" || third.Items[0].MIMEType != driveDeletedMIME {
		t.Fatalf("delete observations = %#v, error = %v", third, err)
	}
	state, err := decodeDriveCursor(third.NextCursor)
	if err != nil || state.Mode != "changes" || state.Token != "current" {
		t.Fatalf("post-delete checkpoint = %#v, error = %v", state, err)
	}
}

func TestGDriveExpiredChangesTokenStartsBoundedRescan(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	rescans, lists := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/changes":
			rescans++
			w.WriteHeader(http.StatusGone)
		case "/drive/changes/startPageToken":
			fmt.Fprint(w, `{"startPageToken":"fresh"}`)
		case "/drive/files":
			lists++
			fmt.Fprint(w, `{"files":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive", "folder_id": "root"}}
	a := NewGDriveAdapter(&Service{httpClient: server.Client()})
	cursor, _ := encodeDriveCursor(driveCursor{Mode: "changes", Token: "expired", InScope: []string{"stale"}})
	page, err := a.Content(context.Background(), c, cursor, 10)
	state, decodeErr := decodeDriveCursor(page.NextCursor)
	if err != nil || decodeErr != nil || rescans != 1 || lists != 1 || !state.Rescan || state.Mode != "deletes" || state.Token != "fresh" {
		t.Fatalf("rescan page=%#v cursor=%#v err=%v decode=%v rescans=%d lists=%d", page, state, err, decodeErr, rescans, lists)
	}
	deletes, err := a.Content(context.Background(), c, page.NextCursor, 10)
	state, decodeErr = decodeDriveCursor(deletes.NextCursor)
	if err != nil || decodeErr != nil || len(deletes.Items) != 1 || deletes.Items[0].NativeID != "stale" || deletes.Items[0].MIMEType != driveDeletedMIME || state.Mode != "changes" || state.Token != "fresh" {
		t.Fatalf("rescan deletions=%#v cursor=%#v err=%v decode=%v", deletes, state, err, decodeErr)
	}
}

func TestGDriveAccessMissingPermissionsIsIncomplete(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"files":[{"id":"hidden"}]}`)
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL, "folder_id": "root"}}
	page, err := NewGDriveAdapter(&Service{httpClient: server.Client()}).Access(context.Background(), c, "", 10)
	if err != nil || page.Completeness != CompletenessIncomplete || page.Snapshot {
		t.Fatalf("access page=%#v error=%v", page, err)
	}
}

func TestGDriveOversizedFileFailsExplicitly(t *testing.T) {
	t.Setenv("CONNECTOR_GOOGLE_DRIVE_TOKEN", "fixture-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/changes/startPageToken":
			fmt.Fprint(w, `{"startPageToken":"start"}`)
		case "/drive/files":
			fmt.Fprint(w, `{"files":[{"id":"large","name":"large.pdf","mimeType":"application/pdf"}]}`)
		case "/drive/files/large":
			_, _ = w.Write(make([]byte, driveMaxResponse+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := source.Connection{Kind: SourceTypeGoogleDrive, CredentialRef: "CONNECTOR_GOOGLE_DRIVE_TOKEN", Config: map[string]any{"base_url": server.URL + "/drive", "folder_id": "root"}}
	page, err := NewGDriveAdapter(&Service{httpClient: server.Client()}).Content(context.Background(), c, "", 10)
	if err == nil || page.Completeness != CompletenessIncomplete || !strings.Contains(err.Error(), "exceeds configured bound") {
		t.Fatalf("oversized result page=%#v error=%v", page, err)
	}
}
