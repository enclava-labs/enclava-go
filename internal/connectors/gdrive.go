package connectors

import (
	"context"
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
	defer resp.Body.Close()
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
	drivePageSize    = 100
	driveMaxResponse = 8 << 20
	driveGoogleSheet = "application/vnd.google-apps.spreadsheet"
	driveShortcut    = "application/vnd.google-apps.shortcut"
)

type DriveAdapter struct{ service *Service }

func NewGDriveAdapter(service *Service) Adapter { return DriveAdapter{service: service} }

func (DriveAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, UnsupportedScopes: []string{"membership"}}
}

func (a DriveAdapter) request(ctx context.Context, c source.Connection, method, endpoint string) (*http.Response, error) {
	view := connectionView(c)
	if c.Kind != string(SourceTypeGoogleDrive) {
		return nil, errors.New("google_drive adapter kind does not match connection")
	}
	token, err := connectorToken(view)
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

func (a DriveAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if limit <= 0 || limit > drivePageSize {
		return ContentPage{}, errors.New("google_drive page limit must be between 1 and 100")
	}
	root, base := driveRoot(c), driveBase(c)
	if root == "" || base == "" {
		return ContentPage{}, errors.New("google_drive config.folder_id and config.base_url are required")
	}
	q := url.Values{"q": {"'" + strings.ReplaceAll(root, "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {strconv.Itoa(limit)}, "fields": {"nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime,headRevisionId,parents,shortcutDetails,exportLinks,size)"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	if cursor != "" {
		q.Set("pageToken", string(cursor))
	}
	resp, err := a.request(ctx, c, http.MethodGet, base+"/files?"+q.Encode())
	if err != nil {
		return ContentPage{}, err
	}
	body, err := readDriveBody(resp)
	if err != nil {
		return ContentPage{}, err
	}
	var listing struct {
		Next  string `json:"nextPageToken"`
		Files []struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			MimeType     string   `json:"mimeType"`
			WebViewLink  string   `json:"webViewLink"`
			ModifiedTime string   `json:"modifiedTime"`
			Revision     string   `json:"headRevisionId"`
			Parents      []string `json:"parents"`
			Size         string   `json:"size"`
			Shortcut     struct {
				TargetID string `json:"targetId"`
			} `json:"shortcutDetails"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return ContentPage{}, fmt.Errorf("decode google_drive listing: %w", err)
	}
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: true, NextCursor: Cursor(listing.Next)}
	for _, f := range listing.Files {
		if f.ID == "" || f.Name == "" {
			return ContentPage{}, errors.New("google_drive file payload is malformed")
		}
		if f.MimeType == driveShortcut {
			// Shortcuts are references, not independent content; never follow out of scope.
			continue
		}
		content, mime := []byte(nil), f.MimeType
		switch f.MimeType {
		case driveGoogleDoc:
			content, mime, err = a.get(ctx, c, base+"/files/"+url.PathEscape(f.ID)+"/export?mimeType=application%2Fvnd.openxmlformats-officedocument.wordprocessingml.document", driveMaxResponse)
			mime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		case driveGoogleSheet:
			content, mime, err = a.sheet(ctx, c, f.ID, f.Name)
		default:
			content, err = a.get(ctx, c, base+"/files/"+url.PathEscape(f.ID)+"?alt=media&supportsAllDrives=true", driveMaxResponse)
		}
		if err != nil {
			return ContentPage{}, fmt.Errorf("google_drive file %s: %w", f.ID, err)
		}
		if len(content) >= driveMaxResponse {
			return ContentPage{}, fmt.Errorf("google_drive file %s exceeds the %d-byte content bound", f.ID, driveMaxResponse)
		}
		tm, _ := time.Parse(time.RFC3339, f.ModifiedTime)
		item := ContentItem{NativeID: f.ID, Kind: c.Kind, NativeRevision: f.Revision, Filename: f.Name, MIMEType: mime, Content: content, URL: f.WebViewLink}
		if len(f.Parents) > 0 {
			item.ParentNativeID = f.Parents[0]
		}
		if !tm.IsZero() {
			item.UpdatedAt = &tm
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func (a DriveAdapter) get(ctx context.Context, c source.Connection, endpoint string, max int64) ([]byte, error) {
	resp, err := a.request(ctx, c, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
					RowCount, ColumnCount int `json:"rowCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(meta, &book); err != nil {
		return nil, "", err
	}
	out := struct {
		Name   string `json:"name"`
		Sheets []any  `json:"sheets"`
	}{Name: name}
	cells := 0
	for _, tab := range book.Sheets {
		p := tab.Properties
		rows := p.GridProperties.RowCount
		cols := p.GridProperties.ColumnCount
		if rows < 0 || cols < 0 || rows > 10000 || cols > 1000 || cells+rows*cols > 100000 {
			return nil, "", errors.New("google_drive spreadsheet exceeds bounded cell range")
		}
		cells += rows * cols
		rng := url.QueryEscape("'" + strings.ReplaceAll(p.Title, "'", "''") + "'!A1:" + driveColumn(cols) + strconv.Itoa(rows))
		resp, err := a.request(ctx, c, http.MethodGet, base+"/spreadsheets/"+url.PathEscape(id)+"/values/"+rng)
		if err != nil {
			return nil, "", err
		}
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
	q := url.Values{"q": {"'" + strings.ReplaceAll(root, "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {strconv.Itoa(limit)}, "fields": {"nextPageToken,files(id,permissions(id,type,emailAddress,domain,role,permissionDetails(inherited,inheritedFrom,permissionType,role)))"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	if cursor != "" {
		q.Set("pageToken", string(cursor))
	}
	resp, err := a.request(ctx, c, http.MethodGet, base+"/files?"+q.Encode())
	if err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	b, err := readDriveBody(resp)
	if err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	var payload struct {
		Next  string `json:"nextPageToken"`
		Files []struct {
			ID          string `json:"id"`
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
	page := AccessPage{Completeness: CompletenessComplete, Snapshot: true, NextCursor: Cursor(payload.Next)}
	for _, f := range payload.Files {
		if f.Permissions == nil {
			page.Completeness = CompletenessIncomplete
		}
		if f.ID == "" {
			return AccessPage{Completeness: CompletenessIncomplete}, errors.New("google_drive permission file is missing id")
		}
		for _, p := range f.Permissions {
			if p.Type == "" || p.ID == "" || p.Role == "" {
				page.Completeness = CompletenessIncomplete
				continue
			}
			inherited, role, inheritedFrom := false, p.Role, ""
			for _, d := range p.Details {
				if d.Inherited {
					inherited, role, inheritedFrom = true, d.Role, d.InheritedFrom
				}
			}
			grant := AccessGrant{NativeObjectID: f.ID, NativeObjectKind: "file", NativePrincipalID: p.EmailAddress, Scope: role, Effect: "allow", Inherited: inherited}
			switch p.Type {
			case "user":
				if grant.NativePrincipalID == "" {
					grant.NativePrincipalID = p.ID
				}
			case "group":
				grant.NativeGroupID = firstNonEmpty(p.EmailAddress, p.ID)
			case "domain":
				grant.NativePrincipalID = p.Domain
			case "anyone":
				grant.NativePrincipalID = "anyone"
			default:
				page.Completeness = CompletenessIncomplete
				continue
			}
			if inheritedFrom != "" {
				grant.Scope += " (inherited from " + inheritedFrom + ")"
			}
			page.Grants = append(page.Grants, grant)
		}
	}
	return page, nil
}

func (DriveAdapter) Membership(context.Context, source.Connection, Cursor, int) (MembershipPage, error) {
	return MembershipPage{Completeness: CompletenessIncomplete}, errors.New("google_drive membership enumeration requires an explicitly authorized directory integration")
}
