package state

// SQL fragments that omit fixture-replay runs from daily counts.
// The plane writes metadata.simulation as JSON boolean true.
//
// COALESCE is load-bearing on SQLite: json_extract of a missing key is
// NULL, and NULL <> 'true' is NULL, which would drop ordinary runs from
// COUNT. Do not "simplify" these predicates.

const SQLExcludeSimulationPostgres = ` AND COALESCE(metadata->>'simulation','') <> 'true'`

const SQLExcludeSimulationMySQL = ` AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata,'$.simulation')),'') <> 'true'`

const SQLExcludeSimulationSQLite = ` AND COALESCE(json_extract(metadata,'$.simulation'), 0) IS NOT 1 AND COALESCE(json_extract(metadata,'$.simulation'), '') <> 'true'`
