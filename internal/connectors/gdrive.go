package connectors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
