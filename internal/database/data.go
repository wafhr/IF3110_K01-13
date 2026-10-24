package database

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

type PageOptions struct {
	Limit  int
	Offset int64
}

func NormalizePageOptions(options PageOptions) (PageOptions, error) {
	if options.Limit == 0 {
		options.Limit = DefaultPageLimit
	}
	if options.Limit < 1 || options.Limit > MaxPageLimit || options.Offset < 0 {
		return PageOptions{}, ErrInvalidInput
	}
	return options, nil
}

type DataColumn struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

// DataPage preserves SQL values as PostgreSQL text or null. In particular,
// bigint/numeric values are not converted to floating-point JSON numbers.
type DataPage struct {
	Schema  string       `json:"schema"`
	Table   string       `json:"table"`
	Columns []DataColumn `json:"columns"`
	Rows    [][]*string  `json:"rows"`
	Limit   int          `json:"limit"`
	Offset  int64        `json:"offset"`
	HasMore bool         `json:"has_more"`
}
