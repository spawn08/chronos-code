// Package contracts turns service contracts into graph nodes (M8): contract
// files (Protobuf/gRPC, Thrift, GraphQL, OpenAPI/Swagger, SQL DDL) declare
// routes, RPCs, messages and tables, and framework recognisers (YAML rules
// in recognisers.yaml) find where source code serves a contract (a route
// registration, a gRPC server, a Kafka consumer) and where it uses one (an
// HTTP client call, a gRPC stub call, a Kafka producer, a SQL query).
//
// A contract node is a facts.Symbol whose Kind is one of the contract kinds
// and whose Name is the contract's global key (see RouteKey and RPCKey).
// A served contract is declared in the serving file with a RefCall from
// the node to its handler; a use is a facts.RefContract naming the key. At
// query time a client call therefore reaches the route, and the route its
// handler. Everything here reads one file.
package contracts

import (
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Languages recorded as facts.File.Lang for contract files.
const (
	LangProto   = "proto"
	LangThrift  = "thrift"
	LangGraphQL = "graphql"
	LangOpenAPI = "openapi"
	LangSQL     = "sql"
)

// Kind returns the contract language of a root-relative path, or "".
// OpenAPI documents are recognised by name: openapi*, swagger* or
// *.openapi with a .yaml, .yml or .json extension.
func Kind(rel string) string {
	base := strings.ToLower(path.Base(rel))
	ext := path.Ext(base)
	switch ext {
	case ".proto":
		return LangProto
	case ".thrift":
		return LangThrift
	case ".graphql", ".graphqls", ".gql":
		return LangGraphQL
	case ".sql":
		return LangSQL
	case ".yaml", ".yml", ".json":
		stem := strings.TrimSuffix(base, ext)
		if strings.HasPrefix(stem, "openapi") || strings.HasPrefix(stem, "swagger") || strings.HasSuffix(stem, ".openapi") {
			return LangOpenAPI
		}
	}
	return ""
}

// HTTP methods a route key can carry; AnyMethod matches every method.
const AnyMethod = "ANY"

var methods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, "HEAD": true, "OPTIONS": true,
}

// Methods lists the concrete HTTP methods, for looking up ANY routes.
var Methods = []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"}

var (
	schemeHost = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/]*`)
	// Path parameters and client-side interpolations, in every syntax a
	// recogniser meets: {id} ${id} :id <int:id> [id] %s %d %v * and
	// numeric or UUID literals (a concrete client path).
	paramSeg  = regexp.MustCompile(`^(?:\{[^/]*\}|\$\{[^/]*\}|:[A-Za-z_]\w*\??|<[^/>]*>|\[[^/\]]*\]|%[sdvq]|\*\w*|\d+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
	paramPart = regexp.MustCompile(`\$\{[^}]*\}|\{[^}]*\}|<[^>]*>|%[sdvq]`)
)

// RouteKey returns the global key of an HTTP route: the upper-case method
// (AnyMethod when unknown) and the path without scheme, host, query or
// trailing slash, with every path parameter written {}:
// RouteKey("get", "https://api/v1/users/:id?x=1") = "GET /v1/users/{}".
// A path may carry its method ("GET /users/{id}", Go 1.22 patterns). It
// returns "" when raw is not a path.
func RouteKey(method, raw string) string {
	raw = strings.TrimSpace(raw)
	if m, rest, ok := strings.Cut(raw, " "); ok && methods[strings.ToUpper(m)] {
		method, raw = m, strings.TrimSpace(rest)
	}
	raw = schemeHost.ReplaceAllString(raw, "")
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	// A leading interpolation is a base URL: ${API}/users, {base}/users,
	// %s/users.
	if strings.HasPrefix(raw, "${") || strings.HasPrefix(raw, "{") {
		if i := strings.Index(raw, "}"); i >= 0 {
			raw = raw[i+1:]
		}
	} else if len(raw) > 2 && raw[0] == '%' && (raw[1] == 's' || raw[1] == 'v') {
		raw = raw[2:]
	}
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	var segs []string
	for _, s := range strings.Split(raw, "/") {
		switch {
		case s == "":
		case paramSeg.MatchString(s):
			segs = append(segs, "{}")
		default:
			segs = append(segs, paramPart.ReplaceAllString(s, "{}"))
		}
	}
	return normMethod(method) + " /" + strings.Join(segs, "/")
}

func normMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if methods[m] {
		return m
	}
	return AnyMethod
}

// SplitRoute splits a route key into its method and path.
func SplitRoute(key string) (method, p string) {
	method, p, _ = strings.Cut(key, " ")
	return method, p
}

// JoinPath joins a route prefix (a class-level or router mount path) and a
// route path.
func JoinPath(prefix, p string) string {
	if prefix == "" {
		return p
	}
	if p == "" || p == "/" {
		return prefix
	}
	return strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(p, "/")
}

// RPCKey returns the global key of an RPC or GraphQL operation:
// "Service/Method" with both parts' first letter upper-cased, so a proto
// GetUser, a Java stub's getUser and a GraphQL query field user meet
// (GraphQL fields are keyed "Query/User"). The package is not part of the
// key: clients rarely know it. It is the contract file's PkgName.
func RPCKey(service, method string) string {
	service, method = lastPart(service), strings.TrimSpace(method)
	if service == "" || method == "" {
		return ""
	}
	return title(service) + "/" + title(method)
}

func lastPart(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func title(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// TableKey returns the global key of a table: its unquoted, lower-case
// name without schema.
func TableKey(name string) string {
	name = strings.Trim(strings.TrimSpace(name), "`\"[]")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = strings.Trim(name[i+1:], "`\"[]")
	}
	return strings.ToLower(name)
}
