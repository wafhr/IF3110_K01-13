package database

import "errors"

var ErrObjectNotFound = errors.New("schema or table not found")

// AccessibleDatabase exposes selection metadata without connection credentials.
type AccessibleDatabase struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DatabaseName string `json:"database_name"`
	Permission   string `json:"permission"`
}

type Schema struct {
	Name string `json:"name"`
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Type   string `json:"type"`
}

type Column struct {
	Name                 string  `json:"name"`
	Position             int     `json:"position"`
	DataType             string  `json:"data_type"`
	Nullable             bool    `json:"nullable"`
	Default              *string `json:"default"`
	Identity             string  `json:"identity"`
	Generated            string  `json:"generated"`
	GenerationExpression *string `json:"generation_expression"`
}

type Constraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Definition string `json:"definition"`
	Validated  bool   `json:"validated"`
}

type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
	Valid      bool   `json:"valid"`
}

type TableDetails struct {
	Table
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints"`
	Indexes     []Index      `json:"indexes"`
}
