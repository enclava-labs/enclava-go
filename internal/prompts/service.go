package prompts

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"text/template"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

// Repository is the tenant-scoped persistence boundary for prompts.Service
// over the prompt_templates domain table. Implemented by postgres.PromptsRepo.
type Repository interface {
	Create(ctx context.Context, t Template) error
	List(ctx context.Context, tenantID string) ([]Template, error)
	GetByID(ctx context.Context, tenantID, id string) (Template, error)
	Update(ctx context.Context, t Template) error
	Delete(ctx context.Context, tenantID, id string) error
}

// VisibleTemplateIDs resolves the prompt template IDs attached to chatbots the
// principal may see. Implemented by chatbot.Service — prompts never reads
// chatbot storage itself.
type VisibleTemplateIDs interface {
	VisiblePromptTemplateIDs(ctx context.Context, p auth.Principal) (map[string]struct{}, error)
}

type Service struct {
	repo      Repository
	chatbots  VisibleTemplateIDs
}

func NewService(repo Repository, chatbots VisibleTemplateIDs) *Service {
	return &Service{repo: repo, chatbots: chatbots}
}

func requireTenant(p auth.Principal) error {
	if strings.TrimSpace(p.TenantID) == "" {
		return errors.Join(source.ErrDenied, errors.New("principal tenant is required"))
	}
	return nil
}

func (s *Service) Create(ctx context.Context, p auth.Principal, input CreateTemplateInput) (Template, error) {
	if err := requireTenant(p); err != nil {
		return Template{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return Template{}, errors.New("name is required")
	}
	userPrompt := strings.TrimSpace(input.UserPrompt)
	if userPrompt == "" {
		userPrompt = "{{.user_input}}"
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = newID("prompt")
	}
	active := true
	if input.IsActive != nil {
		active = *input.IsActive
	}
	t := Template{
		TenantID:        p.TenantID,
		ID:              id,
		Name:            name,
		Description:     strings.TrimSpace(input.Description),
		SystemPrompt:    strings.TrimSpace(input.SystemPrompt),
		UserPrompt:      userPrompt,
		VariablesSchema: nonNilMap(input.VariablesSchema),
		IsDefault:       input.IsDefault,
		IsActive:        active,
		OwnerActorID:    p.ActorID,
	}
	if err := s.repo.Create(ctx, t); err != nil {
		return Template{}, err
	}
	return s.repo.GetByID(ctx, p.TenantID, id)
}

// List returns the tenant's templates filtered through the chatbot allowlist:
// an open chatbot grant sees every template, otherwise only templates attached
// to visible chatbots. Unknown and hidden ids share the same outcome.
func (s *Service) List(ctx context.Context, p auth.Principal) ([]Template, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	templates, err := s.repo.List(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if auth.ChatbotAllowed(p) {
		return templates, nil
	}
	allowed, err := s.visibleIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(templates))
	for _, t := range templates {
		if _, ok := allowed[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, p auth.Principal, id string) (Template, error) {
	if err := requireTenant(p); err != nil {
		return Template{}, err
	}
	t, err := s.repo.GetByID(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Template{}, err
	}
	if !auth.ChatbotAllowed(p) {
		allowed, err := s.visibleIDs(ctx, p)
		if err != nil {
			return Template{}, err
		}
		if _, ok := allowed[t.ID]; !ok {
			return Template{}, sql.ErrNoRows
		}
	}
	return t, nil
}

func (s *Service) visibleIDs(ctx context.Context, p auth.Principal) (map[string]struct{}, error) {
	if s.chatbots == nil {
		return map[string]struct{}{}, nil
	}
	return s.chatbots.VisiblePromptTemplateIDs(ctx, p)
}

func (s *Service) Update(ctx context.Context, p auth.Principal, id string, input UpdateTemplateInput) (Template, error) {
	if err := requireTenant(p); err != nil {
		return Template{}, err
	}
	t, err := s.repo.GetByID(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Template{}, err
	}
	if input.Name != nil {
		t.Name = strings.TrimSpace(*input.Name)
	}
	if t.Name == "" {
		return Template{}, errors.New("name is required")
	}
	if input.Description != nil {
		t.Description = strings.TrimSpace(*input.Description)
	}
	if input.SystemPrompt != nil {
		t.SystemPrompt = strings.TrimSpace(*input.SystemPrompt)
	}
	if input.UserPrompt != nil {
		t.UserPrompt = strings.TrimSpace(*input.UserPrompt)
	}
	if t.UserPrompt == "" {
		t.UserPrompt = "{{.user_input}}"
	}
	if input.VariablesSchema != nil {
		t.VariablesSchema = input.VariablesSchema
	}
	if input.IsDefault != nil {
		t.IsDefault = *input.IsDefault
	}
	if input.IsActive != nil {
		t.IsActive = *input.IsActive
	}
	if err := s.repo.Update(ctx, t); err != nil {
		return Template{}, err
	}
	return s.repo.GetByID(ctx, p.TenantID, t.ID)
}

func (s *Service) Delete(ctx context.Context, p auth.Principal, id string) error {
	if err := requireTenant(p); err != nil {
		return err
	}
	return s.repo.Delete(ctx, p.TenantID, strings.TrimSpace(id))
}

func Render(t Template, input RenderInput) (RenderedPrompt, error) {
	vars := nonNilMap(input.Variables)
	vars["user_input"] = input.UserInput
	vars["question"] = input.UserInput
	vars["rag_context"] = input.RAGContext

	system, err := renderTemplate(t.ID+":system", t.SystemPrompt, vars)
	if err != nil {
		return RenderedPrompt{}, err
	}
	user, err := renderTemplate(t.ID+":user", t.UserPrompt, vars)
	if err != nil {
		return RenderedPrompt{}, err
	}
	return RenderedPrompt{SystemPrompt: strings.TrimSpace(system), UserPrompt: strings.TrimSpace(user)}, nil
}

func renderTemplate(name, body string, vars map[string]any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func nonNilMap(v map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range v {
		out[key] = value
	}
	return out
}
