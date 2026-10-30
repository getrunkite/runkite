package sqlite

import (
	"database/sql/driver"
	"encoding/json"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	modernsqlite "modernc.org/sqlite"
)

func init() {
	// JSON1's json_extract converts numbers to SQLite INTEGER/REAL before a
	// predicate can compare them. Compare raw JSON tokens from -> instead so
	// neither an unsigned integer nor a long decimal can alias its neighbor.
	modernsqlite.MustRegisterDeterministicScalarFunction("runkite_json_number_equal", 2,
		func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if args[0] == nil || args[1] == nil {
				return int64(0), nil
			}
			leftJSON, leftOK := args[0].(string)
			rightJSON, rightOK := args[1].(string)
			if !leftOK || !rightOK {
				return int64(0), nil
			}
			var left, right interface{}
			if err := jsonvalue.Unmarshal([]byte(leftJSON), &left); err != nil {
				return nil, err
			}
			if err := jsonvalue.Unmarshal([]byte(rightJSON), &right); err != nil {
				return nil, err
			}
			leftNumber, leftOK := left.(json.Number)
			rightNumber, rightOK := right.(json.Number)
			if leftOK && rightOK && jsonvalue.EqualNumbers(leftNumber, rightNumber) {
				return int64(1), nil
			}
			return int64(0), nil
		})
}
