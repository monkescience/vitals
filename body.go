package vital

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

const defaultMaxBodySize = 1024 * 1024 // 1MB

// DecodeOption configures body decoding behavior.
type DecodeOption func(*decodeConfig)

type decodeConfig struct {
	maxBodySize int64
}

// WithMaxBodySize sets a custom body size limit.
func WithMaxBodySize(size int64) DecodeOption {
	return func(c *decodeConfig) {
		c.maxBodySize = size
	}
}

// DecodeJSON decodes a JSON request body into type T with validation.
func DecodeJSON[T any](r *http.Request, opts ...DecodeOption) (T, error) {
	var zero T

	config := decodeConfig{
		maxBodySize: defaultMaxBodySize,
	}

	for _, opt := range opts {
		opt(&config)
	}

	limitedReader := io.LimitReader(r.Body, config.maxBodySize+1)
	decoder := json.NewDecoder(limitedReader)

	var result T
	if err := decoder.Decode(&result); err != nil {
		if errors.Is(err, io.EOF) {
			return zero, fmt.Errorf("empty request body")
		}

		if decoder.More() {
			var buf [1]byte
			if _, readErr := limitedReader.Read(buf[:]); readErr == nil {
				return zero, fmt.Errorf("request body exceeds maximum size of %d bytes", config.maxBodySize)
			}
		}

		return zero, fmt.Errorf("invalid JSON: %w", err)
	}

	var buf [1]byte
	if n, _ := limitedReader.Read(buf[:]); n > 0 {
		return zero, fmt.Errorf("request body exceeds maximum size of %d bytes", config.maxBodySize)
	}

	if err := validateRequired(result); err != nil {
		return zero, err
	}

	return result, nil
}

// DecodeForm decodes a form urlencoded request body into type T with validation.
func DecodeForm[T any](r *http.Request, opts ...DecodeOption) (T, error) {
	var zero T

	config := decodeConfig{
		maxBodySize: defaultMaxBodySize,
	}

	for _, opt := range opts {
		opt(&config)
	}

	r.Body = http.MaxBytesReader(nil, r.Body, config.maxBodySize)

	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return zero, fmt.Errorf("request body exceeds maximum size of %d bytes", config.maxBodySize)
		}

		return zero, fmt.Errorf("invalid form data: %w", err)
	}

	var result T
	if err := decodeFormToStruct(r.Form, &result); err != nil {
		return zero, err
	}

	if err := validateRequired(result); err != nil {
		return zero, err
	}

	return result, nil
}

func decodeFormToStruct(form map[string][]string, target any) error {
	val := reflect.ValueOf(target).Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		if !field.CanSet() {
			continue
		}

		formTag := fieldType.Tag.Get("form")
		if formTag == "" {
			formTag = strings.ToLower(fieldType.Name)
		}

		formValues, exists := form[formTag]
		if !exists || len(formValues) == 0 {
			continue
		}

		formValue := formValues[0]

		switch field.Kind() {
		case reflect.String:
			field.SetString(formValue)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			intVal, err := strconv.ParseInt(formValue, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid integer value for field %s: %w", fieldType.Name, err)
			}
			field.SetInt(intVal)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			uintVal, err := strconv.ParseUint(formValue, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid unsigned integer value for field %s: %w", fieldType.Name, err)
			}
			field.SetUint(uintVal)
		case reflect.Float32, reflect.Float64:
			floatVal, err := strconv.ParseFloat(formValue, 64)
			if err != nil {
				return fmt.Errorf("invalid float value for field %s: %w", fieldType.Name, err)
			}
			field.SetFloat(floatVal)
		case reflect.Bool:
			boolVal, err := strconv.ParseBool(formValue)
			if err != nil {
				return fmt.Errorf("invalid boolean value for field %s: %w", fieldType.Name, err)
			}
			field.SetBool(boolVal)
		}
	}

	return nil
}

func validateRequired(v any) error {
	val := reflect.ValueOf(v)
	typ := val.Type()

	var missingFields []string

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		requiredTag := fieldType.Tag.Get("required")
		if requiredTag != "true" {
			continue
		}

		if isZeroValue(field) {
			fieldName := getFieldName(fieldType)
			missingFields = append(missingFields, fieldName)
		}
	}

	if len(missingFields) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missingFields, ", "))
	}

	return nil
}

func isZeroValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.String() == ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

func getFieldName(field reflect.StructField) string {
	if jsonTag := field.Tag.Get("json"); jsonTag != "" {
		parts := strings.Split(jsonTag, ",")
		if parts[0] != "" && parts[0] != "-" {
			return parts[0]
		}
	}

	if formTag := field.Tag.Get("form"); formTag != "" {
		return formTag
	}

	return strings.ToLower(field.Name)
}
