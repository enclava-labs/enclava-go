package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const teamsMaxItems = 1000

func (s *Service) fetchTeamsDocuments(ctx context.Context, source Source) ([]connectorDocument, error) {
	teamID := strings.TrimSpace(stringFromAny(source.Config["team_id"]))
	tenantID := strings.TrimSpace(stringFromAny(source.Config["tenant_id"]))
	clientID := strings.TrimSpace(stringFromAny(source.Config["client_id"]))
	if teamID == "" {
		return nil, errors.New("teams config.team_id is required")
	}
	token, err := connectorToken(source)
	if err != nil {
		return nil, err
	}
	if tenantID != "" || clientID != "" {
		if tenantID == "" || clientID == "" || token == "" {
			return nil, errors.New("teams config.tenant_id, config.client_id, and credential_ref client secret are required together")
		}
		token, err = s.teamsAccessToken(ctx, source, tenantID, clientID, token)
		if err != nil {
			return nil, err
		}
	} else if token == "" {
		return nil, errors.New("teams credential_ref is required")
	}
	baseURL := strings.TrimRight(connectorBaseURL(source, "https://graph.microsoft.com/v1.0"), "/")
	getPage := func(endpoint string) ([]json.RawMessage, string, error) {
		if !connectorSameOrigin(baseURL, endpoint) {
			return nil, "", errors.New("teams pagination URL changed origin")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		var p struct {
			Value []json.RawMessage `json:"value"`
			Next  string            `json:"@odata.nextLink"`
		}
		if err := s.doJSON(req, &p); err != nil {
			return nil, "", err
		}
		return p.Value, p.Next, nil
	}
	var docs []connectorDocument
	channelsURL := baseURL + "/teams/" + url.PathEscape(teamID) + "/channels?$top=100"
	for pages := 0; channelsURL != ""; pages++ {
		if pages >= 100 {
			return nil, errors.New("teams pagination exceeded 100 pages")
		}
		channels, next, err := getPage(channelsURL)
		if err != nil {
			return nil, err
		}
		for _, raw := range channels {
			var channel struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &channel); err != nil || channel.ID == "" {
				return nil, errors.New("teams channel payload is malformed")
			}
			msgURL := baseURL + "/teams/" + url.PathEscape(teamID) + "/channels/" + url.PathEscape(channel.ID) + "/messages?$top=50"
			for mp := 0; msgURL != ""; mp++ {
				if mp >= 100 || len(docs) >= teamsMaxItems {
					return nil, errors.New("teams message pagination exceeded configured bound")
				}
				messages, mnext, err := getPage(msgURL)
				if err != nil {
					return nil, err
				}
				for _, mr := range messages {
					var m struct {
						ID   string `json:"id"`
						Body struct {
							Content string `json:"content"`
						} `json:"body"`
						Modified    string `json:"lastModifiedDateTime"`
						WebURL      string `json:"webUrl"`
						Attachments []struct {
							Name        string `json:"name"`
							ContentType string `json:"contentType"`
							ContentURL  string `json:"contentUrl"`
						} `json:"attachments"`
					}
					if err := json.Unmarshal(mr, &m); err != nil || m.ID == "" {
						return nil, errors.New("teams message payload is malformed")
					}
					content := strings.TrimSpace(m.Body.Content)
					for _, a := range m.Attachments {
						content += fmt.Sprintf("\nAttachment: %s (%s) %s", a.Name, a.ContentType, a.ContentURL)
					}
					if content == "" {
						continue
					}
					repliesURL := baseURL + "/teams/" + url.PathEscape(teamID) + "/channels/" + url.PathEscape(channel.ID) + "/messages/" + url.PathEscape(m.ID) + "/replies?$top=50"
					for rp := 0; repliesURL != ""; rp++ {
						if rp >= 100 {
							return nil, errors.New("teams replies pagination exceeded 100 pages")
						}
						replies, rnext, err := getPage(repliesURL)
						if err != nil {
							return nil, err
						}
						for _, rr := range replies {
							var reply struct {
								ID   string `json:"id"`
								Body struct {
									Content string `json:"content"`
								} `json:"body"`
								Modified string `json:"lastModifiedDateTime"`
							}
							if err := json.Unmarshal(rr, &reply); err != nil || reply.ID == "" {
								return nil, errors.New("teams reply payload is malformed")
							}
							content += "\nReply: " + strings.TrimSpace(reply.Body.Content)
							if reply.Modified > m.Modified {
								m.Modified = reply.Modified
							}
						}
						repliesURL = rnext
					}
					if len(docs) >= teamsMaxItems {
						return nil, errors.New("teams message count exceeded configured bound")
					}
					docs = append(docs, connectorDocument{FileName: "teams-" + teamID + "-" + channel.ID + "-" + m.ID + ".txt", MimeType: "text/plain", Content: content, SourceID: "teams:" + teamID + ":" + channel.ID + ":" + m.ID, SourceURI: m.WebURL, Revision: m.Modified})
				}
				msgURL = mnext
			}
		}
		channelsURL = next
	}
	return docs, nil
}

func (s *Service) teamsAccessToken(ctx context.Context, source Source, tenantID, clientID, secret string) (string, error) {
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(tenantID) + "/oauth2/v2.0/token"
	form := url.Values{"client_id": {clientID}, "client_secret": {secret}, "scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.doJSON(req, &result); err != nil {
		return "", err
	}
	if result.AccessToken == "" {
		return "", errors.New("teams token response omitted access_token")
	}
	return result.AccessToken, nil
}
func connectorSameOrigin(base, endpoint string) bool {
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	e, err := url.Parse(endpoint)
	return err == nil && e.IsAbs() && strings.EqualFold(b.Scheme, e.Scheme) && strings.EqualFold(b.Host, e.Host)
}
