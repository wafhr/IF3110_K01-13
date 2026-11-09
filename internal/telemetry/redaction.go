package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

const Redacted = "[REDACTED]"

var (
	keySeparators      = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")
	credentialURL      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\r\n@]*@`)
	mysqlCredentials   = regexp.MustCompile(`(?i)@(?:tcp|unix)\(`)
	authorizationValue = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+\S+`)
	assignedField      = regexp.MustCompile(`(?i)["']?([a-z_][a-z0-9_.-]*)["']?\s*[:=]`)
)

// Redactor menyimpan aturan immutable yang dapat dipakai bersama antar-goroutine.
type Redactor struct {
	secrets *strings.Replacer
}

func NewRedactor(secrets ...string) *Redactor {
	known := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		known = append(known, secret)
		// Password URL dapat muncul terpisah dan sudah di-decode pada error driver.
		parsed, err := url.Parse(secret)
		if err == nil && parsed.User != nil {
			if password, exists := parsed.User.Password(); exists && password != "" {
				known = append(known, password)
			}
		}
	}
	sort.SliceStable(known, func(left, right int) bool { return len(known[left]) > len(known[right]) })
	replacements := make([]string, 0, len(known)*2)
	for _, secret := range known {
		replacements = append(replacements, secret, Redacted)
	}
	return &Redactor{secrets: strings.NewReplacer(replacements...)}
}

// String menyamarkan secret literal. Teks yang mengandung pola credential
// disamarkan seluruhnya untuk melindungi variasi quoting dan format driver.
func (r *Redactor) String(value string) string {
	if r.secrets != nil {
		value = r.secrets.Replace(value)
	}
	if credentialURL.MatchString(value) || mysqlCredentials.MatchString(value) || authorizationValue.MatchString(value) {
		return Redacted
	}
	for _, match := range assignedField.FindAllStringSubmatch(value, -1) {
		if sensitiveKey(match[1]) {
			return Redacted
		}
	}
	return value
}

// ReplaceAttr digunakan oleh handler slog. Nama grup ikut diperiksa karena
// handler memanggil ReplaceAttr pada atribut anggota grup.
func (r *Redactor) ReplaceAttr(groups []string, attr slog.Attr) slog.Attr {
	for _, group := range groups {
		if sensitiveKey(group) {
			return slog.String(attr.Key, Redacted)
		}
	}
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, Redacted)
	}
	attr.Value = attr.Value.Resolve()
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(r.String(attr.Value.String()))
	case slog.KindAny:
		attr.Value = slog.AnyValue(r.redactAny(attr.Value.Any()))
	}
	return attr
}

func sensitiveKey(key string) bool {
	normalized := keySeparators.Replace(strings.ToLower(key))
	switch normalized {
	case "authorization", "proxyauthorization", "cookie", "setcookie",
		"session", "sessions", "sessionid", "sessionids", "sessionidentifier", "sessiondata", "currenttokenduringgrace",
		"credential", "credentials", "body", "requestbody", "responsebody", "payload",
		"args", "arguments", "params", "parameters", "sql", "sqlup", "sqldown", "query", "querytext", "statement",
		"email", "phone", "phonenumber", "sensitive", "sensitivedata":
		return true
	}
	for _, suffix := range []string{
		"username", "password", "passwordhash", "passwordciphertext", "passwd", "pwd",
		"token", "tokenhash", "secret", "apikey", "accesskey", "privatekey", "encryptionkey",
		"connectionstring", "connectionurl", "databaseurl", "databaseuri", "dsn",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func (r *Redactor) redactAny(value any) (result any) {
	// Nilai yang gagal diserialisasi atau memicu panic disamarkan sepenuhnya.
	defer func() {
		if recover() != nil {
			result = Redacted
		}
	}()
	if err, ok := value.(error); ok {
		return r.String(err.Error())
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Redacted
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return Redacted
	}
	return r.redactJSON(result)
}

func (r *Redactor) redactJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if sensitiveKey(key) {
				value[key] = Redacted
			} else {
				value[key] = r.redactJSON(item)
			}
		}
		return value
	case []any:
		for index, item := range value {
			value[index] = r.redactJSON(item)
		}
		return value
	case string:
		return r.String(value)
	default:
		return value
	}
}

func (r *Redactor) traceAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	sanitized := append([]attribute.KeyValue(nil), attrs...)
	for index, attr := range sanitized {
		if sensitiveKey(string(attr.Key)) {
			sanitized[index].Value = attribute.StringValue(Redacted)
		} else {
			sanitized[index].Value = r.traceValue(attr.Value)
		}
	}
	return sanitized
}

func (r *Redactor) traceValue(value attribute.Value) attribute.Value {
	switch value.Type() {
	case attribute.STRING:
		return attribute.StringValue(r.String(value.AsString()))
	case attribute.STRINGSLICE:
		values := value.AsStringSlice()
		for index := range values {
			values[index] = r.String(values[index])
		}
		return attribute.StringSliceValue(values)
	case attribute.SLICE:
		values := value.AsSlice()
		for index := range values {
			values[index] = r.traceValue(values[index])
		}
		return attribute.SliceValue(values...)
	case attribute.MAP:
		return attribute.MapValue(r.traceAttributes(value.AsMap())...)
	case attribute.BYTESLICE:
		// Data biner disamarkan karena isinya tidak memiliki label field.
		return attribute.StringValue(Redacted)
	default:
		return value
	}
}
