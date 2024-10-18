package scalar

import (
	"encoding/json"
	"fmt"
	"strconv"
)

func NewID[T any](v T) ID[T] {
	return ID[T]{Value: v}
}

// ID represents GraphQL's "ID" scalar type. A custom type may be used instead.
type ID[T any] struct {
	Value T
}

func (ID[T]) ImplementsGraphQLType(name string) bool {
	return name == "ID"
}

func (id *ID[T]) UnmarshalGraphQL(v any) error {
	if data, err := json.Marshal(v); err != nil {
		return err
	} else if err := json.Unmarshal(data, &id.Value); err != nil {
		return err
	}

	return nil
}

func (id ID[T]) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, fmt.Sprintf("%v", id.Value)), nil
}
