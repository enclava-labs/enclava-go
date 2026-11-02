package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var publicRoutePatterns = map[string]string{
	"/":                             "Root redirects to the dashboard.",
	"/health":                       "Health endpoint serves operational status.",
	"/livez":                        "Liveness probe must work before authentication.",
	"/readyz":                       "Readiness probe must work before authentication.",
	"/metrics":                      "Metrics enforce their own bearer token.",
	"/dashboard/login":              "Login must be accessible before a session exists.",
	"/dashboard/logout":             "Logout clears a session and cookies.",
	"/dashboard/static/":            "Login and error pages need static assets.",
	"/auth/oidc/":                   "OIDC start and callback establish a session.",
	"/auth/oidc/link":               "Link confirmation verifies the link token and account credentials before issuing a session.",
	"/dashboard/partials/evidence/": "This fall-through renders only a not-found page.",
}

var routeAuthWrappers = map[string]bool{
	"authenticated":                true,
	"Authenticated":                true,
	"requireCap":                   true,
	"capSession":                   true,
	"RequireSession":               true,
	"RequireSessionScope":          true,
	"handleDashboardAccessTab":     true,
	"handleDashboardAccessPartial": true,
}

func routeRegistration(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") || len(call.Args) != 2 {
		return false
	}
	switch receiver := selector.X.(type) {
	case *ast.Ident:
		return receiver.Name == "mux"
	case *ast.SelectorExpr:
		return receiver.Sel.Name == "mux"
	default:
		return false
	}
}

func routeHasAuth(handler ast.Expr) bool {
	found := false
	ast.Inspect(handler, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			found = found || routeAuthWrappers[fn.Name]
		case *ast.SelectorExpr:
			found = found || routeAuthWrappers[fn.Sel.Name]
		}
		return !found
	})
	return found
}

func publicRoute(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	pattern, err := strconv.Unquote(literal.Value)
	if err != nil {
		return false
	}
	if fields := strings.Fields(pattern); len(fields) == 2 {
		pattern = fields[1]
	}
	_, ok = publicRoutePatterns[pattern]
	return ok
}

func TestRouteAuthorization(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok && routeRegistration(call) && !publicRoute(call.Args[0]) && !routeHasAuth(call.Args[1]) {
				t.Errorf("%s: route handler has no recognized auth wrapper", fset.Position(call.Pos()))
			}
			return true
		})
	}
}

func TestRouteAuthorizationDetection(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{`mux.HandleFunc("/private/"+name, app.Authenticated("read", handler))`, true},
		{`a.mux.Handle("/private", http.HandlerFunc(handler))`, false},
		{`mux.HandleFunc("/private", wrap(a.sessionAuth.requireCap(capRead, handler)))`, true},
	} {
		expr, err := parser.ParseExpr(tc.source)
		if err != nil {
			t.Fatal(err)
		}
		call := expr.(*ast.CallExpr)
		if !routeRegistration(call) || routeHasAuth(call.Args[1]) != tc.want {
			t.Errorf("route authorization detection for %s", tc.source)
		}
	}
}
