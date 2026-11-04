package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/source"
)

const driveMaxFiles = 1000
const driveGoogleDoc = "application/vnd.google-apps.document"

func (s *Service) fetchGoogleDriveDocuments(ctx context.Context, source Source) ([]connectorDocument, error) {
	folderID := strings.TrimSpace(stringFromAny(source.Config["folder_id"]))
	if folderID == "" {
		return nil, errors.New("google_drive config.folder_id is required")
	}
	token, err := connectorToken(source)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("google_drive credential_ref is required")
	}
	baseURL := strings.TrimRight(connectorBaseURL(source, "https://www.googleapis.com/drive/v3"), "/")
	endpoint := baseURL + "/files?" + url.Values{"q": {"'" + strings.ReplaceAll(folderID, "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {"100"}, "fields": {"nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime,headRevisionId)"}}.Encode()
	client := s.connectorHTTPClient()
	docs := make([]connectorDocument, 0)
	for pages := 0; endpoint != ""; pages++ {
		if pages >= 100 {
			return nil, errors.New("google_drive pagination exceeded 100 pages")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		var payload struct {
			Files []struct {
				ID           string `json:"id"`
				Name         string `json:"name"`
				MimeType     string `json:"mimeType"`
				WebViewLink  string `json:"webViewLink"`
				ModifiedTime string `json:"modifiedTime"`
				Revision     string `json:"headRevisionId"`
			} `json:"files"`
			Next string `json:"nextPageToken"`
		}
		if err := s.doJSON(req, &payload); err != nil {
			return nil, err
		}
		for _, file := range payload.Files {
			if file.ID == "" || file.Name == "" {
				return nil, errors.New("google_drive file payload is malformed")
			}
			content := fmt.Sprintf("Name: %s\nType: %s\nURI: %s\nModified: %s", file.Name, file.MimeType, file.WebViewLink, file.ModifiedTime)
			mimeType := "text/plain"
			if file.MimeType == driveGoogleDoc {
				export := baseURL + "/files/" + url.PathEscape(file.ID) + "/export?mimeType=text%2Fplain"
				exportReq, err := http.NewRequestWithContext(ctx, http.MethodGet, export, nil)
				if err != nil {
					return nil, err
				}
				exportReq.Header.Set("Authorization", "Bearer "+token)
				resp, err := client.Do(exportReq)
				if err != nil {
					return nil, fmt.Errorf("google_drive export request failed: %w", err)
				}
				defer func() { _ = resp.Body.Close() }()
				body, readErr := readConnectorBody(resp)
				if readErr != nil {
					return nil, readErr
				}
				content, mimeType = string(body), "text/plain"
			}
			if len(docs) >= driveMaxFiles {
				return nil, errors.New("google_drive file count exceeded configured bound")
			}
			docs = append(docs, connectorDocument{FileName: file.Name, MimeType: mimeType, Content: content, SourceID: "google_drive:" + file.ID, SourceURI: firstNonEmpty(file.WebViewLink, "https://drive.google.com/file/d/"+url.PathEscape(file.ID)+"/view"), Revision: file.Revision})
		}
		if payload.Next == "" {
			endpoint = ""
		} else {
			q := url.Values{"pageToken": {payload.Next}, "pageSize": {"100"}, "fields": {"nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime,headRevisionId)"}}
			q.Set("q", "'"+strings.ReplaceAll(folderID, "'", "\\'")+"' in parents and trashed = false")
			endpoint = baseURL + "/files?" + q.Encode()
		}
	}
	return docs, nil
}

func readConnectorBody(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read connector response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("connector upstream returned status %d", resp.StatusCode)
	}
	return body, nil
}

const (
	drivePageSize     = 100
	driveMaxResponse  = 8 << 20
	driveGoogleSheet  = "application/vnd.google-apps.spreadsheet"
	driveGoogleFolder = "application/vnd.google-apps.folder"
	driveShortcut     = "application/vnd.google-apps.shortcut"
)

type DriveAdapter struct{ service *Service }

func NewGDriveAdapter(service *Service) Adapter { return DriveAdapter{service: service} }

func (DriveAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, UnsupportedScopes: []string{"membership"}}
}

func (a DriveAdapter) request(ctx context.Context, c source.Connection, method, endpoint string) (*http.Response, error) {
	if c.Kind != string(SourceTypeGoogleDrive) {
		return nil, errors.New("google_drive adapter kind does not match connection")
	}
	token, err := a.service.connectorCredential(c, endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.service.connectorHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("google_drive request failed: %w", err)
	}
	return resp, nil
}

func driveBase(c source.Connection) string {
	return strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["base_url"]), "https://www.googleapis.com/drive/v3"), "/")
}

func driveRoot(c source.Connection) string {
	return strings.TrimSpace(stringFromAny(c.Config["folder_id"]))
}
func driveSharedID(c source.Connection) string {
	return firstNonEmpty(strings.TrimSpace(stringFromAny(c.Config["shared_drive_id"])), strings.TrimSpace(stringFromAny(c.Config["drive_id"])))
}
func driveTypeAllowed(c source.Connection, mimeType string) bool {
	allowed := stringListFromAny(c.Config["mime_types"])
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.TrimSpace(candidate) == mimeType {
			return true
		}
	}
	return false
}

func (a DriveAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if limit <= 0 || limit > drivePageSize {
		return ContentPage{}, errors.New("google_drive page limit must be between 1 and 100")
	}
	if driveRoot(c) == "" {
		return ContentPage{}, errors.New("google_drive config.folder_id is required")
	}
	state, err := decodeDriveCursor(cursor)
	if err != nil {
		return ContentPage{}, err
	}
	switch state.Mode {
	case "", "files":
		return a.driveFiles(ctx, c, state, limit)
	case "changes":
		return a.driveChanges(ctx, c, state, limit)
	case "deletes":
		end := limit
		if end > len(state.Removed) {
			end = len(state.Removed)
		}
		page := ContentPage{Completeness: CompletenessComplete, Snapshot: false}
		for _, id := range state.Removed[:end] {
			page.Items = append(page.Items, ContentItem{NativeID: id, Kind: c.Kind, MIMEType: driveDeletedMIME})
		}
		state.Removed = state.Removed[end:]
		if len(state.Removed) > 0 {
			page.NextCursor, err = encodeDriveCursor(state)
		} else {
			page.Checkpoint, err = encodeDriveCursor(driveCursor{Mode: "changes", Token: state.Token, InScope: state.InScope})
		}
		return page, err
	default:
		return ContentPage{}, errors.New("google_drive cursor mode is invalid")
	}
}

const driveDeletedMIME = "application/x-enclava-deleted"

type driveCursor struct {
	Mode    string   `json:"m"`
	Token   string   `json:"t,omitempty"`
	Start   string   `json:"s,omitempty"`
	Rescan  bool     `json:"r,omitempty"`
	InScope []string `json:"i,omitempty"`
	Removed []string `json:"d,omitempty"`
	Prior   []string `json:"p,omitempty"`
}

func decodeDriveCursor(c Cursor) (driveCursor, error) {
	if c == "" {
		return driveCursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return driveCursor{}, errors.New("google_drive cursor is invalid")
	}
	var state driveCursor
	if err := json.Unmarshal(b, &state); err != nil || state.Mode == "" || len(state.InScope) > driveMaxFiles || len(state.Removed) > driveMaxFiles || len(state.Prior) > driveMaxFiles {
		return driveCursor{}, errors.New("google_drive cursor is invalid or exceeds its bound")
	}
	return state, nil
}

func encodeDriveCursor(c driveCursor) (Cursor, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return Cursor(base64.RawURLEncoding.EncodeToString(b)), nil
}

func (a DriveAdapter) driveFiles(ctx context.Context, c source.Connection, state driveCursor, limit int) (ContentPage, error) {
	base := driveBase(c)
	if state.Start == "" {
		token, err := a.driveStartToken(ctx, c)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, err
		}
		state.Start = token
	}
	q := url.Values{"q": {"'" + strings.ReplaceAll(driveRoot(c), "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {strconv.Itoa(limit)}, "fields": {"nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime,headRevisionId,parents,shortcutDetails)"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	if driveID := driveSharedID(c); driveID != "" {
		q.Set("corpora", "drive")
		q.Set("driveId", driveID)
	}
	if state.Token != "" {
		q.Set("pageToken", state.Token)
	}
	resp, err := a.request(ctx, c, http.MethodGet, base+"/files?"+q.Encode())
	if err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readDriveBody(resp)
	if err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, err
	}
	var listing struct {
		Next  string      `json:"nextPageToken"`
		Files []driveFile `json:"files"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, fmt.Errorf("decode google_drive listing: %w", err)
	}
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: listing.Next == ""}
	for _, f := range listing.Files {
		if !driveTypeAllowed(c, f.MimeType) {
			continue
		}
		item, err := a.driveItem(ctx, c, f)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, err
		}
		if item == nil {
			continue
		}
		page.Items = append(page.Items, *item)
		state.InScope = appendDriveID(state.InScope, f.ID)
		if len(state.InScope) > driveMaxFiles {
			return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive selected scope exceeds configured bound")
		}
	}
	if listing.Next != "" {
		state.Mode, state.Token = "files", listing.Next
	} else {
		state.Mode, state.Token = "changes", state.Start
		state.Start = ""
		if state.Rescan {
			for _, id := range state.Prior {
				if !containsDriveID(state.InScope, id) {
					state.Removed = appendDriveID(state.Removed, id)
				}
			}
			state.Prior = nil
			if len(state.Removed) > driveMaxFiles {
				return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive rescan deletion set exceeds configured bound")
			}
			if len(state.Removed) > 0 {
				state.Mode = "deletes"
			}
		}
	}
	page.NextCursor, err = encodeDriveCursor(state)
	return page, err
}

type driveFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	WebViewLink  string   `json:"webViewLink"`
	ModifiedTime string   `json:"modifiedTime"`
	Revision     string   `json:"headRevisionId"`
	Parents      []string `json:"parents"`
	Trashed      bool     `json:"trashed"`
}

func appendDriveID(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

func (a DriveAdapter) driveItem(ctx context.Context, c source.Connection, f driveFile) (*ContentItem, error) {
	if f.ID == "" || f.Name == "" {
		return nil, errors.New("google_drive file payload is malformed")
	}
	if f.MimeType == driveGoogleFolder {
		return nil, nil
	}
	if f.MimeType == driveShortcut {
		return nil, nil
	}
	base := driveBase(c)
	content, mime := []byte(nil), f.MimeType
	var err error
	switch f.MimeType {
	case driveGoogleDoc:
		content, err = a.get(ctx, c, base+"/files/"+url.PathEscape(f.ID)+"/export?mimeType=application%2Fvnd.openxmlformats-officedocument.wordprocessingml.document", driveMaxResponse)
		mime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case driveGoogleSheet:
		content, mime, err = a.sheet(ctx, c, f.ID, f.Name)
	default:
		content, err = a.get(ctx, c, base+"/files/"+url.PathEscape(f.ID)+"?alt=media&supportsAllDrives=true", driveMaxResponse)
	}
	if err != nil {
		return nil, fmt.Errorf("google_drive file %s: %w", f.ID, err)
	}
	tm, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	item := &ContentItem{NativeID: f.ID, Kind: c.Kind, NativeRevision: f.Revision, Title: f.Name, Filename: f.Name, MIMEType: mime, Content: content, URL: f.WebViewLink}
	if len(f.Parents) > 0 {
		item.ParentNativeID = f.Parents[0]
	}
	if !tm.IsZero() {
		item.UpdatedAt = &tm
	}
	return item, nil
}

func (a DriveAdapter) driveStartToken(ctx context.Context, c source.Connection) (string, error) {
	q := url.Values{"supportsAllDrives": {"true"}}
	if driveID := driveSharedID(c); driveID != "" {
		q.Set("driveId", driveID)
	}
	endpoint := driveBase(c) + "/changes/startPageToken?" + q.Encode()
	resp, err := a.request(ctx, c, http.MethodGet, endpoint)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readDriveBody(resp)
	if err != nil {
		return "", err
	}
	var start struct {
		Token string `json:"startPageToken"`
	}
	if err := json.Unmarshal(b, &start); err != nil || start.Token == "" {
		return "", errors.New("google_drive start page token is missing")
	}
	return start.Token, nil
}

func (a DriveAdapter) driveChanges(ctx context.Context, c source.Connection, state driveCursor, limit int) (ContentPage, error) {
	q := url.Values{"pageToken": {state.Token}, "pageSize": {strconv.Itoa(limit)}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}, "fields": {"nextPageToken,newStartPageToken,changes(fileId,removed,file(id,name,mimeType,webViewLink,modifiedTime,headRevisionId,parents,trashed))"}}
	if driveID := driveSharedID(c); driveID != "" {
		q.Set("driveId", driveID)
	}
	resp, err := a.request(ctx, c, http.MethodGet, driveBase(c)+"/changes?"+q.Encode())
	if err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, err
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusGone {
		_ = resp.Body.Close()
		// An expired checkpoint restarts a bounded root listing and carries the rescan marker in its cursor.
		state.Prior = state.InScope
		state.Mode, state.Token, state.Start, state.InScope, state.Removed, state.Rescan = "files", "", "", nil, nil, true
		return a.driveFiles(ctx, c, state, limit)
	}
	b, err := readDriveBody(resp)
	if err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, err
	}
	var payload struct {
		Next    string `json:"nextPageToken"`
		Start   string `json:"newStartPageToken"`
		Changes []struct {
			FileID  string    `json:"fileId"`
			Removed bool      `json:"removed"`
			File    driveFile `json:"file"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return ContentPage{Completeness: CompletenessIncomplete}, err
	}
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: false}
	for _, change := range payload.Changes {
		id := firstNonEmpty(change.FileID, change.File.ID)
		if id == "" {
			return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive change is missing file id")
		}
		if change.Removed || change.File.Trashed {
			if containsDriveID(state.InScope, id) {
				state.Removed = appendDriveID(state.Removed, id)
				state.InScope = removeDriveID(state.InScope, id)
				if len(state.Removed) > driveMaxFiles {
					return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive pending deletion set exceeds configured bound")
				}
			}
			continue
		}
		inScope := containsDriveID(state.InScope, id)
		if inScope && change.File.Parents == nil {
			return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive change omitted parents for a scoped file")
		}
		if !containsDriveID(change.File.Parents, driveRoot(c)) {
			if inScope {
				state.Removed = appendDriveID(state.Removed, id)
				state.InScope = removeDriveID(state.InScope, id)
				if len(state.Removed) > driveMaxFiles {
					return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive pending deletion set exceeds configured bound")
				}
			}
			continue
		}
		if !driveTypeAllowed(c, change.File.MimeType) {
			if inScope {
				state.Removed = appendDriveID(state.Removed, id)
				state.InScope = removeDriveID(state.InScope, id)
			}
			continue
		}
		item, err := a.driveItem(ctx, c, change.File)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, err
		}
		if item != nil {
			page.Items = append(page.Items, *item)
			state.InScope = appendDriveID(state.InScope, id)
			if len(state.InScope) > driveMaxFiles {
				return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive selected scope exceeds configured bound")
			}
		}
		if len(state.Removed) > driveMaxFiles {
			return ContentPage{Completeness: CompletenessIncomplete}, errors.New("google_drive pending deletion set exceeds configured bound")
		}
	}
	if payload.Next != "" {
		state.Token = payload.Next
	} else {
		state.Token = firstNonEmpty(payload.Start, state.Token)
	}
	if len(state.Removed) > 0 && payload.Next == "" {
		state.Mode = "deletes"
	} else {
		state.Mode = "changes"
	}
	if payload.Next == "" && len(state.Removed) == 0 {
		page.Checkpoint, err = encodeDriveCursor(state)
	} else {
		page.NextCursor, err = encodeDriveCursor(state)
	}
	return page, err
}

func containsDriveID(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

func removeDriveID(ids []string, id string) []string {
	out := ids[:0]
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	return out
}

func (a DriveAdapter) get(ctx context.Context, c source.Connection, endpoint string, max int64) ([]byte, error) {
	resp, err := a.request(ctx, c, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("google_drive upstream returned status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("google_drive response exceeds configured bound")
	}
	return b, nil
}

func readDriveBody(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("google_drive upstream returned status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, driveMaxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(b) > driveMaxResponse {
		return nil, errors.New("google_drive response exceeds configured bound")
	}
	return b, nil
}

func (a DriveAdapter) sheet(ctx context.Context, c source.Connection, id, name string) ([]byte, string, error) {
	base := strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["sheets_base_url"]), "https://sheets.googleapis.com/v4"), "/")
	resp, err := a.request(ctx, c, http.MethodGet, base+"/spreadsheets/"+url.PathEscape(id)+"?includeGridData=false")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	meta, err := readDriveBody(resp)
	if err != nil {
		return nil, "", err
	}
	var book struct {
		Sheets []struct {
			Properties struct {
				SheetID        int64  `json:"sheetId"`
				Title          string `json:"title"`
				GridProperties struct {
					RowCount    int `json:"rowCount"`
					ColumnCount int `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(meta, &book); err != nil {
		return nil, "", err
	}
	out := struct {
		Name       string `json:"name"`
		NativeMIME string `json:"native_mime"`
		ExportMIME string `json:"export_mime"`
		Sheets     []any  `json:"sheets"`
	}{Name: name, NativeMIME: driveGoogleSheet, ExportMIME: "application/json"}
	cells := 0
	for _, tab := range book.Sheets {
		p := tab.Properties
		rows := p.GridProperties.RowCount
		cols := p.GridProperties.ColumnCount
		if rows < 0 || cols < 0 || rows > 10000 || cols > 1000 || cells+rows*cols > 100000 {
			return nil, "", errors.New("google_drive spreadsheet exceeds bounded cell range")
		}
		cells += rows * cols
		rng := url.PathEscape("'" + strings.ReplaceAll(p.Title, "'", "''") + "'!A1:" + driveColumn(cols) + strconv.Itoa(rows))
		resp, err := a.request(ctx, c, http.MethodGet, base+"/spreadsheets/"+url.PathEscape(id)+"/values/"+rng)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := readDriveBody(resp)
		if err != nil {
			return nil, "", err
		}
		var values struct {
			Values [][]any `json:"values"`
		}
		if err := json.Unmarshal(b, &values); err != nil {
			return nil, "", err
		}
		out.Sheets = append(out.Sheets, map[string]any{"sheet_id": p.SheetID, "title": p.Title, "range": "A1:" + driveColumn(cols) + strconv.Itoa(rows), "values": values.Values})
	}
	b, err := json.Marshal(out)
	return b, "application/json", err
}

func driveColumn(n int) string {
	if n < 1 {
		return "A"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		n--
		i--
		b[i] = byte('A' + n%26)
		n /= 26
	}
	return string(b[i:])
}

func (a DriveAdapter) Access(ctx context.Context, c source.Connection, cursor Cursor, limit int) (AccessPage, error) {
	if limit <= 0 || limit > drivePageSize {
		return AccessPage{}, errors.New("google_drive page limit must be between 1 and 100")
	}
	base, root := driveBase(c), driveRoot(c)
	if base == "" || root == "" {
		return AccessPage{Completeness: CompletenessIncomplete}, errors.New("google_drive access scope is not configured")
	}
	q := url.Values{"q": {"'" + strings.ReplaceAll(root, "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {strconv.Itoa(limit)}, "fields": {"nextPageToken,files(id,driveId,inheritedPermissionsDisabled,copyRequiresWriterPermission,downloadRestrictions(itemDownloadRestriction(restrictedForReaders,restrictedForWriters)),permissions(id,type,emailAddress,domain,role,permissionDetails(inherited,inheritedFrom,permissionType,role)))"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	if driveID := driveSharedID(c); driveID != "" {
		q.Set("corpora", "drive")
		q.Set("driveId", driveID)
	}
	if cursor != "" {
		q.Set("pageToken", string(cursor))
	}
	resp, err := a.request(ctx, c, http.MethodGet, base+"/files?"+q.Encode())
	if err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readDriveBody(resp)
	if err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	var payload struct {
		Next  string `json:"nextPageToken"`
		Files []struct {
			ID                           string `json:"id"`
			DriveID                      string `json:"driveId"`
			InheritedPermissionsDisabled *bool  `json:"inheritedPermissionsDisabled"`
			CopyRequiresWriterPermission *bool  `json:"copyRequiresWriterPermission"`
			DownloadRestrictions         struct {
				Item struct {
					RestrictedForReaders *bool `json:"restrictedForReaders"`
					RestrictedForWriters *bool `json:"restrictedForWriters"`
				} `json:"itemDownloadRestriction"`
			} `json:"downloadRestrictions"`
			Permissions []struct {
				ID, Type, EmailAddress, Domain, Role string
				Details                              []struct {
					Inherited                           bool `json:"inherited"`
					InheritedFrom, PermissionType, Role string
				} `json:"permissionDetails"`
			} `json:"permissions"`
		} `json:"files"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	page := AccessPage{Completeness: CompletenessComplete, Snapshot: payload.Next == "", NextCursor: Cursor(payload.Next)}
	for _, f := range payload.Files {
		if f.Permissions == nil {
			page.Completeness = CompletenessIncomplete
		}
		if f.ID == "" {
			return AccessPage{Completeness: CompletenessIncomplete}, errors.New("google_drive permission file is missing id")
		}
		if f.DriveID != "" && (f.InheritedPermissionsDisabled == nil || f.CopyRequiresWriterPermission == nil || f.DownloadRestrictions.Item.RestrictedForReaders == nil || f.DownloadRestrictions.Item.RestrictedForWriters == nil) {
			page.Completeness = CompletenessIncomplete
		}
		if (f.CopyRequiresWriterPermission != nil && *f.CopyRequiresWriterPermission) ||
			(f.DownloadRestrictions.Item.RestrictedForReaders != nil && *f.DownloadRestrictions.Item.RestrictedForReaders) ||
			(f.DownloadRestrictions.Item.RestrictedForWriters != nil && *f.DownloadRestrictions.Item.RestrictedForWriters) {
			page.Grants = append(page.Grants, AccessGrant{NativeObjectID: f.ID, Scope: "copy_download_restricted", Effect: "deny"})
		}
		for _, p := range f.Permissions {
			if p.Type == "" || p.ID == "" || p.Role == "" {
				page.Completeness = CompletenessIncomplete
				continue
			}
			inherited, role := false, p.Role
			for _, d := range p.Details {
				if d.Inherited {
					inherited = true
					if d.Role != "" {
						role = d.Role
					}
				}
			}
			grant := AccessGrant{NativeObjectID: f.ID, NativePrincipalID: p.EmailAddress, Scope: role, Effect: "allow", Inherited: inherited}
			switch p.Type {
			case "user":
				if grant.NativePrincipalID == "" {
					grant.NativePrincipalID = p.ID
				}
			case "group":
				grant.NativePrincipalID = ""
				grant.NativeGroupID = firstNonEmpty(p.EmailAddress, p.ID)
			case "domain":
				grant.NativePrincipalID = p.Domain
			case "anyone":
				grant.NativePrincipalID = "anyone"
			default:
				page.Completeness = CompletenessIncomplete
				continue
			}
			page.Grants = append(page.Grants, grant)
		}
	}
	if page.Completeness != CompletenessComplete {
		page.Snapshot, page.NextCursor = false, ""
	}
	return page, nil
}

func (DriveAdapter) Membership(context.Context, source.Connection, Cursor, int) (MembershipPage, error) {
	return MembershipPage{Completeness: CompletenessIncomplete}, errors.New("google_drive membership enumeration requires an explicitly authorized directory integration")
}
