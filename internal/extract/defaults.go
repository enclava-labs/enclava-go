package extract

func DefaultTemplates() []Template {
	return []Template{
		{
			ID:           "detailed_invoice",
			Name:         "Detailed Invoice",
			Description:  "Invoice extraction with line items, taxes, and party data",
			SystemPrompt: "You extract structured invoice data. Return only valid JSON.",
			UserPrompt: `Extract invoice data from this document.
Use these optional hints when available:
- company_name: {company_name}
- expected_currency: {currency}

Return JSON with fields:
invoice_number, invoice_date, due_date, service_provider, buyer, subtotal, tax_amount, total_amount, currency, line_items.`,
			ContextSchema: map[string]any{
				"company_name": map[string]any{"type": "string", "required": false},
				"currency":     map[string]any{"type": "string", "required": false},
			},
			OutputSchema: map[string]any{
				"type":     "object",
				"required": []any{"invoice_number", "total_amount"},
				"properties": map[string]any{
					"invoice_number":   map[string]any{"type": "string"},
					"invoice_date":     map[string]any{"type": "string"},
					"due_date":         map[string]any{"type": "string"},
					"service_provider": map[string]any{"type": "object"},
					"buyer":            map[string]any{"type": "object"},
					"subtotal":         map[string]any{"type": "number"},
					"tax_amount":       map[string]any{"type": "number"},
					"total_amount":     map[string]any{"type": "number"},
					"currency":         map[string]any{"type": "string"},
					"line_items":       map[string]any{"type": "array"},
				},
			},
			IsDefault: true,
			Enabled:   true,
		},
		{
			ID:           "simple_receipt",
			Name:         "Simple Receipt",
			Description:  "Receipt extraction for merchant, date, items, totals",
			SystemPrompt: "You extract structured receipt data. Return only valid JSON.",
			UserPrompt: `Extract receipt data from this document.
Return JSON with fields:
merchant_name, merchant_address, date, items, subtotal, tax, total, currency, payment_method.`,
			ContextSchema: map[string]any{},
			OutputSchema: map[string]any{
				"type":     "object",
				"required": []any{"merchant_name", "total"},
				"properties": map[string]any{
					"merchant_name":    map[string]any{"type": "string"},
					"merchant_address": map[string]any{"type": "string"},
					"date":             map[string]any{"type": "string"},
					"items":            map[string]any{"type": "array"},
					"subtotal":         map[string]any{"type": "number"},
					"tax":              map[string]any{"type": "number"},
					"total":            map[string]any{"type": "number"},
					"currency":         map[string]any{"type": "string"},
					"payment_method":   map[string]any{"type": "string"},
				},
			},
			IsDefault: true,
			Enabled:   true,
		},
		{
			ID:           "expense_report",
			Name:         "Expense Report",
			Description:  "Expense-focused extraction with normalization fields",
			SystemPrompt: "You extract expense data from invoices/receipts. Return only valid JSON.",
			UserPrompt: `Extract expense data from this document.
Optional hints:
- employee_name: {employee_name}
- department: {department}

Return JSON with fields:
vendor, date, amount, currency, category, description, payment_method, tax_amount, is_reimbursable.`,
			ContextSchema: map[string]any{
				"employee_name": map[string]any{"type": "string", "required": false},
				"department":    map[string]any{"type": "string", "required": false},
			},
			OutputSchema: map[string]any{
				"type":     "object",
				"required": []any{"vendor", "amount", "currency"},
				"properties": map[string]any{
					"vendor":          map[string]any{"type": "string"},
					"date":            map[string]any{"type": "string"},
					"amount":          map[string]any{"type": "number"},
					"currency":        map[string]any{"type": "string"},
					"category":        map[string]any{"type": "string"},
					"description":     map[string]any{"type": "string"},
					"payment_method":  map[string]any{"type": "string"},
					"tax_amount":      map[string]any{"type": "number"},
					"is_reimbursable": map[string]any{"type": "boolean"},
				},
			},
			IsDefault: true,
			Enabled:   true,
		},
	}
}
