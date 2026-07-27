package main

import (
	"fmt"

	apivalidate "github.com/goobers/goobers/api/validate"
)

func validateSchemaJSON(schemaFile string, data []byte) error {
	validator, err := apivalidate.New()
	if err != nil {
		return fmt.Errorf("create schema validator: %w", err)
	}
	if err := validator.ValidateJSON(schemaFile, data); err != nil {
		return fmt.Errorf("validate against %s: %w", schemaFile, err)
	}
	return nil
}
