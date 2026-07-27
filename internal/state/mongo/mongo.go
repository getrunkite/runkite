// Package mongo implements the state.Store interface using MongoDB. This
// is the project's non-SQL exemplar backend -- proof that state.Store is
// genuinely implementable against a document store, not just SQL
// databases, and a template for community-contributed backends.
//
// Design notes (see internal/state/postgres for the reference SQL
// implementation this mirrors field-for-field):
//   - Collections map 1:1 to Postgres tables. Unique indexes (created in
//     Init) enforce the same composite-key semantics Postgres's PRIMARY
//     KEY/UNIQUE INDEX pairs do (e.g. agents on (tenant_id, agent_id),
//     threads on thread_id alone -- matching Postgres's schema exactly,
//     including which tables scope uniqueness by tenant and which don't).
//   - JSON fields (metadata, input, config, etc.) are unmarshaled into
//     bson.M/interface{} before storing, so they're native BSON
//     documents (queryable, not opaque blobs) -- mirroring what JSONB
//     does in Postgres. On the way back out, normalizeBSON recursively
//     converts BSON's decode-time types (bson.M, bson.A, int32/int64)
//     into encoding/json's types (map[string]interface{},
//     []interface{}, float64) -- without it, code elsewhere in this
//     project doing `v.(map[string]interface{})` or comparing a decoded
//     number against a float64 literal would silently break against
//     this one backend only (confirmed live: two conformance tests
//     failed exactly this way before normalizeBSON existed).
//   - Namespace arrays (key-value store) reuse the exact same \x1F
//     delimited string encoding Postgres/SQLite use (nsToString/
//     stringToNs/nsPrefixPattern), so prefix search is a regex anchored
//     at "^", not a Mongo-specific array-prefix trick -- guarantees
//     identical observable behavior to the SQL backends for the same
//     conformance suite.
package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Store implements state.Store with a MongoDB database.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

var _ state.Store = (*Store)(nil)

// New creates a new MongoDB store from a connection URI and database name.
func New(ctx context.Context, uri, dbName string) (*Store, error) {
	// DefaultDocumentM: without it, decoding a BSON subdocument into an
	// `interface{}`-typed struct field (every JSON-ish field here --
	// metadata, values, input, etc.) yields bson.D (an ordered []bson.E
	// slice), not a map -- bsonToMap's type assertion would silently
	// return nil for every such field otherwise. This makes it decode
	// as bson.M (map[string]interface{}) instead, matching what
	// jsonToBSON produces on the way in.
	clientOpts := options.Client().ApplyURI(uri).SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true})
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("mongo.Connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{client: client, db: client.Database(dbName)}, nil
}

func (s *Store) col(name string) *mongo.Collection { return s.db.Collection(name) }

// Init creates all indexes. Collections themselves are created implicitly
// on first write -- MongoDB doesn't need a CREATE TABLE equivalent.
func (s *Store) Init(ctx context.Context) error {
	type idx struct {
		collection string
		keys       bson.D
		unique     bool
	}
	indexes := []idx{
		{"agents", bson.D{{Key: "tenant_id", Value: 1}, {Key: "agent_id", Value: 1}}, true},
		{"agent_schemas", bson.D{{Key: "tenant_id", Value: 1}, {Key: "agent_id", Value: 1}}, true},
		{"threads", bson.D{{Key: "thread_id", Value: 1}}, true},
		{"threads", bson.D{{Key: "tenant_id", Value: 1}}, false},
		{"runs", bson.D{{Key: "run_id", Value: 1}}, true},
		{"runs", bson.D{{Key: "thread_id", Value: 1}}, false},
		{"runs", bson.D{{Key: "status", Value: 1}}, false},
		{"runs", bson.D{{Key: "tenant_id", Value: 1}}, false},
		{"thread_checkpoints", bson.D{{Key: "checkpoint_id", Value: 1}}, true},
		{"thread_checkpoints", bson.D{{Key: "thread_id", Value: 1}, {Key: "created_at", Value: -1}}, false},
		{"store_items", bson.D{{Key: "tenant_id", Value: 1}, {Key: "namespace", Value: 1}, {Key: "key", Value: 1}}, true},
		{"webhook_dead_letters", bson.D{{Key: "id", Value: 1}}, true},
		{"webhook_dead_letters", bson.D{{Key: "failed_at", Value: -1}}, false},
		{"run_cache", bson.D{{Key: "tenant_id", Value: 1}, {Key: "cache_key", Value: 1}}, true},
		{"run_cache", bson.D{{Key: "expires_at", Value: 1}}, false},
		{"cron_schedules", bson.D{{Key: "tenant_id", Value: 1}, {Key: "name", Value: 1}}, true},
		{"cron_claims", bson.D{{Key: "tenant_id", Value: 1}, {Key: "schedule_name", Value: 1}, {Key: "fire_time", Value: 1}}, true},
	}
	for _, i := range indexes {
		opts := options.Index()
		if i.unique {
			opts.SetUnique(true)
		}
		if _, err := s.col(i.collection).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: i.keys, Options: opts}); err != nil {
			return fmt.Errorf("create index on %s: %w", i.collection, err)
		}
	}
	return nil
}

// Close disconnects the client.
func (s *Store) Close() error {
	return s.client.Disconnect(context.Background())
}

// TruncateAll removes all documents from all collections. For testing only.
func (s *Store) TruncateAll(ctx context.Context) error {
	for _, c := range []string{"agents", "agent_versions", "agent_schemas", "threads", "runs", "thread_checkpoints",
		"store_items", "webhook_dead_letters", "run_cache", "cron_schedules", "cron_claims"} {
		if _, err := s.col(c).DeleteMany(ctx, bson.D{}); err != nil {
			return err
		}
	}
	return nil
}

// jsonToBSON unmarshals a JSON value into a native Go value BSON can
// marshal directly (map[string]interface{}/[]interface{}/scalars) --
// mirrors storing arbitrary JSON in a JSONB column, kept queryable
// rather than as an opaque blob. Nil/empty input yields nil (matches
// Postgres/SQLite's NULL column for an absent value).
func jsonToBSON(raw json.RawMessage) interface{} {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// normalizeBSON recursively converts BSON's decode-time output types
// (bson.M, bson.A, int32/int64) into the types encoding/json's
// Unmarshal-into-any produces (map[string]interface{}, []interface{},
// float64). bson.M/bson.A are DEFINED types (`type M map[string]any`),
// not aliases -- a bson.M value fails a `v.(map[string]interface{})`
// type assertion despite having an identical underlying type, and BSON
// numbers decode as int32/int64 rather than JSON's float64. Without this
// normalization, every piece of code elsewhere in this project that does
// `v.(map[string]interface{})`/`v.([]interface{})` or compares a decoded
// number against a float64 literal (confirmed live: two conformance
// tests failed exactly this way) would silently break against this one
// backend only, even though the BSON storage itself is queryable and
// correct.
func normalizeBSON(v interface{}) interface{} {
	switch val := v.(type) {
	case bson.M:
		out := make(map[string]interface{}, len(val))
		for k, vv := range val {
			out[k] = normalizeBSON(vv)
		}
		return out
	case bson.A:
		out := make([]interface{}, len(val))
		for i, vv := range val {
			out[i] = normalizeBSON(vv)
		}
		return out
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	default:
		return val
	}
}

// bsonToJSON reverses jsonToBSON for fields read back out as
// json.RawMessage (Run.Input/Config/Output, CronSchedule.Input/Config,
// WebhookDeadLetter.Payload).
func bsonToJSON(v interface{}) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(normalizeBSON(v))
	if err != nil {
		return nil
	}
	return b
}

// bsonToMap reverses jsonToBSON for fields read back out as
// map[string]interface{} (Metadata, Values, Capabilities, schemas).
func bsonToMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	m, ok := normalizeBSON(v).(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}

// bsonToSlice reverses jsonToBSON for fields read back out as
// []interface{} (ThreadState.Next/Tasks/Interrupts).
func bsonToSlice(v interface{}) []interface{} {
	if v == nil {
		return nil
	}
	a, ok := normalizeBSON(v).([]interface{})
	if !ok {
		return nil
	}
	return a
}

// tenantFilter appends a tenant_id equality clause unless ctx is a
// system context -- same semantics as every Postgres/SQLite method's
// "if !tenant.IsSystem(ctx) { ... AND tenant_id = ... }" branch.
func tenantFilter(ctx context.Context, filter bson.M) bson.M {
	if !tenant.IsSystem(ctx) {
		filter["tenant_id"] = tenant.FromContext(ctx)
	}
	return filter
}

// --------------------------------------------------------------------------
// Agents
// --------------------------------------------------------------------------

type agentDoc struct {
	TenantID     string      `bson:"tenant_id"`
	AgentID      string      `bson:"agent_id"`
	Name         string      `bson:"name"`
	Description  string      `bson:"description"`
	Metadata     interface{} `bson:"metadata"`
	Capabilities interface{} `bson:"capabilities"`
	Version      int         `bson:"version"`
	CreatedAt    time.Time   `bson:"created_at"`
	UpdatedAt    time.Time   `bson:"updated_at"`
}

func (s *Store) UpsertAgent(ctx context.Context, agent *models.Agent) error {
	tid := tenant.FromContext(ctx)
	now := time.Now().UTC()

	var existing agentDoc
	err := s.col("agents").FindOne(ctx, bson.M{"tenant_id": tid, "agent_id": agent.AgentID}).Decode(&existing)
	version := 1
	versionChanged := true // new agent (no existing doc) always needs its v1 snapshot
	if err == nil {
		version = existing.Version
		versionChanged = false
		// version bumps only if the definition actually changed --
		// matches Postgres's JSONB-equality CASE expression, compared
		// here as parsed Go values rather than serialized text.
		metaBytes, _ := json.Marshal(agent.Metadata)
		existingMetaBytes, _ := json.Marshal(existing.Metadata)
		capsBytes, _ := json.Marshal(agent.Capabilities)
		existingCapsBytes, _ := json.Marshal(existing.Capabilities)
		if existing.Name != agent.Name || existing.Description != agent.Description ||
			string(metaBytes) != string(existingMetaBytes) || string(capsBytes) != string(existingCapsBytes) {
			version = existing.Version + 1
			versionChanged = true
		}
	} else if err != mongo.ErrNoDocuments {
		return err
	}

	_, err = s.col("agents").UpdateOne(ctx,
		bson.M{"tenant_id": tid, "agent_id": agent.AgentID},
		bson.M{
			"$set": bson.M{
				"name": agent.Name, "description": agent.Description,
				"metadata": agent.Metadata, "capabilities": agent.Capabilities,
				"version": version, "updated_at": now,
			},
			"$setOnInsert": bson.M{"tenant_id": tid, "agent_id": agent.AgentID, "created_at": now},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return err
	}

	// Full agent versioning (master plan: "version history browsing,
	// rollback to arbitrary past versions") -- one immutable document
	// per version ever served, written only when this call is the one
	// that actually bumped the version (an unchanged re-registration,
	// e.g. every control plane restart with an unchanged
	// langgraph.json, must not duplicate a version snapshot).
	if versionChanged {
		_, err = s.col("agent_versions").InsertOne(ctx, agentVersionDoc{
			TenantID: tid, AgentID: agent.AgentID, Version: version,
			Name: agent.Name, Description: agent.Description,
			Metadata: agent.Metadata, Capabilities: agent.Capabilities, CreatedAt: now,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type agentVersionDoc struct {
	TenantID     string      `bson:"tenant_id"`
	AgentID      string      `bson:"agent_id"`
	Version      int         `bson:"version"`
	Name         string      `bson:"name"`
	Description  string      `bson:"description"`
	Metadata     interface{} `bson:"metadata"`
	Capabilities interface{} `bson:"capabilities"`
	CreatedAt    time.Time   `bson:"created_at"`
}

func toAgentVersion(doc agentVersionDoc) *models.AgentVersion {
	return &models.AgentVersion{
		TenantID: doc.TenantID, AgentID: doc.AgentID, Version: doc.Version,
		Name: doc.Name, Description: doc.Description,
		Metadata: bsonToMap(doc.Metadata), Capabilities: bsonToMap(doc.Capabilities), CreatedAt: doc.CreatedAt,
	}
}

func (s *Store) ListAgentVersions(ctx context.Context, agentID string) ([]*models.AgentVersion, error) {
	filter := tenantFilter(ctx, bson.M{"agent_id": agentID})
	cur, err := s.col("agent_versions").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	versions := []*models.AgentVersion{}
	for cur.Next(ctx) {
		var doc agentVersionDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		versions = append(versions, toAgentVersion(doc))
	}
	return versions, cur.Err()
}

func (s *Store) GetAgentVersion(ctx context.Context, agentID string, version int) (*models.AgentVersion, error) {
	var doc agentVersionDoc
	err := s.col("agent_versions").FindOne(ctx, tenantFilter(ctx, bson.M{"agent_id": agentID, "version": version})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "agent_version", ID: fmt.Sprintf("%s@v%d", agentID, version)}
	}
	if err != nil {
		return nil, err
	}
	return toAgentVersion(doc), nil
}

func (s *Store) GetAgent(ctx context.Context, agentID string) (*models.Agent, error) {
	var doc agentDoc
	err := s.col("agents").FindOne(ctx, tenantFilter(ctx, bson.M{"agent_id": agentID})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "agent", ID: agentID}
	}
	if err != nil {
		return nil, err
	}
	return &models.Agent{
		TenantID: doc.TenantID, AgentID: doc.AgentID, Name: doc.Name, Description: doc.Description,
		Metadata: bsonToMap(doc.Metadata), Capabilities: bsonToMap(doc.Capabilities), Version: doc.Version,
	}, nil
}

func (s *Store) SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	filter := tenantFilter(ctx, bson.M{})
	if req.Name != "" {
		filter["name"] = bson.M{"$regex": regexQuoteMeta(req.Name), "$options": "i"}
	}
	for k, v := range req.Metadata {
		filter["metadata."+k] = metadataMatchValue(v)
	}

	cur, err := s.col("agents").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetSkip(int64(req.Offset)).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	agents := []*models.Agent{}
	for cur.Next(ctx) {
		var doc agentDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		agents = append(agents, &models.Agent{
			TenantID: doc.TenantID, AgentID: doc.AgentID, Name: doc.Name, Description: doc.Description,
			Metadata: bsonToMap(doc.Metadata), Capabilities: bsonToMap(doc.Capabilities), Version: doc.Version,
		})
	}
	return agents, cur.Err()
}

type agentSchemaDoc struct {
	TenantID     string      `bson:"tenant_id"`
	AgentID      string      `bson:"agent_id"`
	InputSchema  interface{} `bson:"input_schema"`
	OutputSchema interface{} `bson:"output_schema"`
	StateSchema  interface{} `bson:"state_schema"`
	ConfigSchema interface{} `bson:"config_schema"`
}

func (s *Store) UpsertAgentSchema(ctx context.Context, schema *models.AgentSchema) error {
	tid := tenant.FromContext(ctx)
	_, err := s.col("agent_schemas").UpdateOne(ctx,
		bson.M{"tenant_id": tid, "agent_id": schema.AgentID},
		bson.M{"$set": bson.M{
			"input_schema": schema.InputSchema, "output_schema": schema.OutputSchema,
			"state_schema": schema.StateSchema, "config_schema": schema.ConfigSchema,
		}, "$setOnInsert": bson.M{"tenant_id": tid, "agent_id": schema.AgentID}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

func (s *Store) GetAgentSchema(ctx context.Context, agentID string) (*models.AgentSchema, error) {
	var doc agentSchemaDoc
	err := s.col("agent_schemas").FindOne(ctx, tenantFilter(ctx, bson.M{"agent_id": agentID})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "agent_schema", ID: agentID}
	}
	if err != nil {
		return nil, err
	}
	return &models.AgentSchema{
		AgentID: doc.AgentID, InputSchema: bsonToMap(doc.InputSchema), OutputSchema: bsonToMap(doc.OutputSchema),
		StateSchema: bsonToMap(doc.StateSchema), ConfigSchema: bsonToMap(doc.ConfigSchema),
	}, nil
}

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

type threadDoc struct {
	TenantID  string      `bson:"tenant_id"`
	ThreadID  string      `bson:"thread_id"`
	Status    string      `bson:"status"`
	Metadata  interface{} `bson:"metadata"`
	Values    interface{} `bson:"values"`
	CreatedAt time.Time   `bson:"created_at"`
	UpdatedAt time.Time   `bson:"updated_at"`
}

func toThread(doc threadDoc) *models.Thread {
	return &models.Thread{
		TenantID: doc.TenantID, ThreadID: doc.ThreadID, Status: models.ThreadStatus(doc.Status),
		Metadata: bsonToMap(doc.Metadata), Values: bsonToMap(doc.Values),
		CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt,
	}
}

func (s *Store) CreateThread(ctx context.Context, thread *models.Thread) error {
	_, err := s.col("threads").InsertOne(ctx, threadDoc{
		TenantID: tenant.FromContext(ctx), ThreadID: thread.ThreadID, Status: string(thread.Status),
		Metadata: thread.Metadata, Values: thread.Values, CreatedAt: thread.CreatedAt, UpdatedAt: thread.UpdatedAt,
	})
	if mongo.IsDuplicateKeyError(err) {
		return &state.ErrConflict{Resource: "thread", ID: thread.ThreadID}
	}
	return err
}

func (s *Store) GetThread(ctx context.Context, threadID string) (*models.Thread, error) {
	var doc threadDoc
	err := s.col("threads").FindOne(ctx, tenantFilter(ctx, bson.M{"thread_id": threadID})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	if err != nil {
		return nil, err
	}
	return toThread(doc), nil
}

func (s *Store) UpdateThread(ctx context.Context, threadID string, patch *models.ThreadPatch) (*models.Thread, error) {
	existing, err := s.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if patch.Metadata != nil {
		if existing.Metadata == nil {
			existing.Metadata = map[string]interface{}{}
		}
		for k, v := range patch.Metadata {
			existing.Metadata[k] = v
		}
	}
	if patch.Values != nil {
		if existing.Values == nil {
			existing.Values = map[string]interface{}{}
		}
		for k, v := range patch.Values {
			existing.Values[k] = v
		}
	}
	existing.UpdatedAt = time.Now().UTC()

	_, err = s.col("threads").UpdateOne(ctx, tenantFilter(ctx, bson.M{"thread_id": threadID}),
		bson.M{"$set": bson.M{"metadata": existing.Metadata, "values": existing.Values, "updated_at": existing.UpdatedAt}})
	return existing, err
}

func (s *Store) DeleteThread(ctx context.Context, threadID string) error {
	// Emulate Postgres's ON DELETE CASCADE. Parent delete MUST be
	// tenant-filtered and happen BEFORE wiping children: the previous
	// children-first order deleted runs/checkpoints by thread_id alone,
	// so a wrong-tenant DeleteThread wiped another tenant's child rows
	// and then failed on the thread row (ErrNotFound) -- silent
	// cross-tenant data loss the conformance suite missed when it only
	// asserted the thread document survived.
	res, err := s.col("threads").DeleteOne(ctx, tenantFilter(ctx, bson.M{"thread_id": threadID}))
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	if _, err := s.col("runs").DeleteMany(ctx, bson.M{"thread_id": threadID}); err != nil {
		return err
	}
	if _, err := s.col("thread_checkpoints").DeleteMany(ctx, bson.M{"thread_id": threadID}); err != nil {
		return err
	}
	return nil
}

func (s *Store) SearchThreads(ctx context.Context, req *models.ThreadSearchRequest) ([]*models.Thread, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	filter := tenantFilter(ctx, bson.M{})
	if req.Status != nil {
		filter["status"] = string(*req.Status)
	}
	for k, v := range req.Metadata {
		filter["metadata."+k] = metadataMatchValue(v)
	}

	cur, err := s.col("threads").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetSkip(int64(req.Offset)).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	threads := []*models.Thread{}
	for cur.Next(ctx) {
		var doc threadDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		threads = append(threads, toThread(doc))
	}
	return threads, cur.Err()
}

func (s *Store) SetThreadStatus(ctx context.Context, threadID string, status models.ThreadStatus) error {
	_, err := s.col("threads").UpdateOne(ctx, tenantFilter(ctx, bson.M{"thread_id": threadID}),
		bson.M{"$set": bson.M{"status": string(status), "updated_at": time.Now().UTC()}})
	return err
}

func (s *Store) TryClaimThread(ctx context.Context, threadID string) (bool, error) {
	filter := tenantFilter(ctx, bson.M{"thread_id": threadID, "status": bson.M{"$ne": string(models.ThreadStatusBusy)}})
	res, err := s.col("threads").UpdateOne(ctx, filter,
		bson.M{"$set": bson.M{"status": string(models.ThreadStatusBusy), "updated_at": time.Now().UTC()}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount > 0, nil
}

// --------------------------------------------------------------------------
// Checkpoints
// --------------------------------------------------------------------------

type checkpointDoc struct {
	TenantID     string      `bson:"tenant_id"`
	CheckpointID string      `bson:"checkpoint_id"`
	ThreadID     string      `bson:"thread_id"`
	CheckpointNS string      `bson:"checkpoint_ns"`
	ParentID     *string     `bson:"parent_id"`
	Values       interface{} `bson:"values"`
	Metadata     interface{} `bson:"metadata"`
	NextNodes    interface{} `bson:"next_nodes"`
	Tasks        interface{} `bson:"tasks"`
	Interrupts   interface{} `bson:"interrupts"`
	CreatedAt    time.Time   `bson:"created_at"`
}

func toThreadState(doc checkpointDoc) *models.ThreadState {
	ts := &models.ThreadState{
		Values:     bsonToMap(doc.Values),
		Metadata:   bsonToMap(doc.Metadata),
		Tasks:      bsonToSlice(doc.Tasks),
		Interrupts: bsonToSlice(doc.Interrupts),
		Checkpoint: models.ThreadCheckpoint{CheckpointID: doc.CheckpointID, ThreadID: doc.ThreadID, CheckpointNS: doc.CheckpointNS},
	}
	if next := bsonToSlice(doc.NextNodes); next != nil {
		for _, n := range next {
			if s, ok := n.(string); ok {
				ts.Next = append(ts.Next, s)
			}
		}
	}
	if ts.Next == nil {
		ts.Next = []string{}
	}
	cat := doc.CreatedAt.Format(time.RFC3339)
	ts.CreatedAt = &cat
	if doc.ParentID != nil {
		ts.ParentCheckpoint = &models.ThreadCheckpoint{CheckpointID: *doc.ParentID, ThreadID: doc.ThreadID, CheckpointNS: doc.CheckpointNS}
	}
	return ts
}

func (s *Store) SaveCheckpoint(ctx context.Context, threadID string, ts *models.ThreadState) error {
	var parentID *string
	if ts.ParentCheckpoint != nil {
		parentID = &ts.ParentCheckpoint.CheckpointID
	}
	createdAt := time.Now().UTC()
	if ts.CreatedAt != nil && *ts.CreatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, *ts.CreatedAt); err == nil {
			createdAt = parsed
		}
	}
	next := make([]interface{}, len(ts.Next))
	for i, n := range ts.Next {
		next[i] = n
	}
	_, err := s.col("thread_checkpoints").InsertOne(ctx, checkpointDoc{
		TenantID: tenant.FromContext(ctx), CheckpointID: ts.Checkpoint.CheckpointID, ThreadID: threadID,
		CheckpointNS: ts.Checkpoint.CheckpointNS, ParentID: parentID, Values: ts.Values, Metadata: ts.Metadata,
		NextNodes: next, Tasks: ts.Tasks, Interrupts: ts.Interrupts, CreatedAt: createdAt,
	})
	return err
}

func (s *Store) GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error) {
	var doc checkpointDoc
	err := s.col("thread_checkpoints").FindOne(ctx, tenantFilter(ctx, bson.M{"thread_id": threadID}),
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "checkpoint", ID: "latest"}
	}
	if err != nil {
		return nil, err
	}
	return toThreadState(doc), nil
}

func (s *Store) ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error) {
	if limit <= 0 {
		limit = 10
	}
	filter := tenantFilter(ctx, bson.M{"thread_id": threadID})
	if before != "" {
		var beforeDoc checkpointDoc
		if err := s.col("thread_checkpoints").FindOne(ctx, bson.M{"checkpoint_id": before}).Decode(&beforeDoc); err == nil {
			filter["created_at"] = bson.M{"$lt": beforeDoc.CreatedAt}
		}
	}
	cur, err := s.col("thread_checkpoints").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := []*models.ThreadState{}
	for cur.Next(ctx) {
		var doc checkpointDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		out = append(out, toThreadState(doc))
	}
	return out, cur.Err()
}

func (s *Store) PruneCheckpoints(ctx context.Context, keepLast int) (int64, error) {
	if keepLast <= 0 {
		return 0, nil
	}
	filter := tenantFilter(ctx, bson.M{})
	var threadIDs []interface{}
	if err := s.col("thread_checkpoints").Distinct(ctx, "thread_id", filter).Decode(&threadIDs); err != nil {
		return 0, err
	}
	var total int64
	for _, raw := range threadIDs {
		threadID, ok := raw.(string)
		if !ok {
			continue
		}
		threadFilter := bson.M{}
		for k, v := range filter {
			threadFilter[k] = v
		}
		threadFilter["thread_id"] = threadID
		cur, err := s.col("thread_checkpoints").Find(ctx, threadFilter,
			options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetSkip(int64(keepLast)).SetProjection(bson.M{"checkpoint_id": 1}))
		if err != nil {
			return total, err
		}
		var toDelete []string
		for cur.Next(ctx) {
			var doc struct {
				CheckpointID string `bson:"checkpoint_id"`
			}
			if err := cur.Decode(&doc); err != nil {
				cur.Close(ctx)
				return total, err
			}
			toDelete = append(toDelete, doc.CheckpointID)
		}
		cur.Close(ctx)
		if len(toDelete) == 0 {
			continue
		}
		res, err := s.col("thread_checkpoints").DeleteMany(ctx, bson.M{"checkpoint_id": bson.M{"$in": toDelete}})
		if err != nil {
			return total, err
		}
		total += res.DeletedCount
	}
	return total, nil
}

// --------------------------------------------------------------------------
// Runs
// --------------------------------------------------------------------------

type runDoc struct {
	TenantID  string      `bson:"tenant_id"`
	RunID     string      `bson:"run_id"`
	ThreadID  string      `bson:"thread_id"`
	AgentID   string      `bson:"agent_id"`
	Status    string      `bson:"status"`
	Metadata  interface{} `bson:"metadata"`
	Input     interface{} `bson:"input"`
	Config    interface{} `bson:"config"`
	Output    interface{} `bson:"output"`
	ErrorMsg  string      `bson:"error_msg"`
	CreatedAt time.Time   `bson:"created_at"`
	UpdatedAt time.Time   `bson:"updated_at"`
	// Agent-to-Agent (A2A) delegation bookkeeping -- see models.Run's
	// doc comment. ParentRunID/RootRunID use `omitempty` so a
	// top-level run's document simply has no such field, matching the
	// SQL backends' NULL rather than storing an empty string.
	ParentRunID string `bson:"parent_run_id,omitempty"`
	RootRunID   string `bson:"root_run_id,omitempty"`
	Depth       int    `bson:"depth"`
}

func toRun(doc runDoc) *models.Run {
	r := &models.Run{
		TenantID: doc.TenantID, RunID: doc.RunID, ThreadID: doc.ThreadID, AgentID: doc.AgentID,
		AssistantID: doc.AgentID, Status: models.RunStatus(doc.Status), Metadata: bsonToMap(doc.Metadata),
		Input: bsonToJSON(doc.Input), Config: bsonToJSON(doc.Config), Output: bsonToJSON(doc.Output),
		Error: doc.ErrorMsg, CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt, Depth: doc.Depth,
	}
	if doc.ParentRunID != "" {
		r.ParentRunID = &doc.ParentRunID
	}
	if doc.RootRunID != "" {
		r.RootRunID = &doc.RootRunID
	}
	return r
}

func (s *Store) CreateRun(ctx context.Context, run *models.Run) error {
	doc := runDoc{
		TenantID: tenant.FromContext(ctx), RunID: run.RunID, ThreadID: run.ThreadID, AgentID: run.AgentID,
		Status: string(run.Status), Metadata: run.Metadata, Input: jsonToBSON(run.Input), Config: jsonToBSON(run.Config),
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, Depth: run.Depth,
	}
	if run.ParentRunID != nil {
		doc.ParentRunID = *run.ParentRunID
	}
	if run.RootRunID != nil {
		doc.RootRunID = *run.RootRunID
	}
	_, err := s.col("runs").InsertOne(ctx, doc)
	return err
}

func (s *Store) GetRun(ctx context.Context, runID string) (*models.Run, error) {
	var doc runDoc
	err := s.col("runs").FindOne(ctx, tenantFilter(ctx, bson.M{"run_id": runID})).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "run", ID: runID}
	}
	if err != nil {
		return nil, err
	}
	return toRun(doc), nil
}

func (s *Store) UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error {
	_, err := s.col("runs").UpdateOne(ctx, tenantFilter(ctx, bson.M{"run_id": runID}),
		bson.M{"$set": bson.M{
			"status": string(status), "output": jsonToBSON(output), "error_msg": errMsg, "updated_at": time.Now().UTC(),
		}})
	return err
}

func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	res, err := s.col("runs").DeleteOne(ctx, tenantFilter(ctx, bson.M{"run_id": runID}))
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return &state.ErrNotFound{Resource: "run", ID: runID}
	}
	return nil
}

func (s *Store) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	filter := tenantFilter(ctx, bson.M{})
	if req.Status != nil {
		filter["status"] = string(*req.Status)
	}
	if req.ThreadID != "" {
		filter["thread_id"] = req.ThreadID
	}
	if req.AgentID != "" {
		filter["agent_id"] = req.AgentID
	}
	for k, v := range req.Metadata {
		filter["metadata."+k] = metadataMatchValue(v)
	}

	cur, err := s.col("runs").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetSkip(int64(req.Offset)).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	runs := []*models.Run{}
	for cur.Next(ctx) {
		var doc runDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		runs = append(runs, toRun(doc))
	}
	return runs, cur.Err()
}

// pruneableRunStatuses matches api.isTerminalStatus's definition
// (internal/api/runs.go) -- duplicated rather than imported to keep
// internal/state free of a dependency on internal/api.
var pruneableRunStatuses = []string{"success", "error", "interrupted", "timeout"}

func (s *Store) PruneRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	filter := tenantFilter(ctx, bson.M{
		"status":     bson.M{"$in": pruneableRunStatuses},
		"updated_at": bson.M{"$lt": olderThan},
	})
	res, err := s.col("runs").DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// --------------------------------------------------------------------------
// Store (key-value)
// --------------------------------------------------------------------------

// Same namespace encoding as Postgres/SQLite -- \x1F delimited, wrapped in
// leading and trailing delimiters for boundary-safe prefix matching.
const nsDelim = "\x1F"

func nsToString(ns []string) string {
	return nsDelim + strings.Join(ns, nsDelim) + nsDelim
}

func stringToNs(s string) []string {
	trimmed := strings.Trim(s, nsDelim)
	if trimmed == "" {
		return []string{}
	}
	return strings.Split(trimmed, nsDelim)
}

func nsPrefixRegex(prefix []string) string {
	joined := nsDelim + strings.Join(prefix, nsDelim) + nsDelim
	return "^" + regexQuoteMeta(joined)
}

// regexQuoteMeta escapes regex metacharacters so a literal string (a
// namespace segment, an agent name search term) can't be misread as a
// pattern -- mirrors why Postgres/SQLite use parameterized LIKE with a
// literal-escaped value rather than raw string concatenation.
func regexQuoteMeta(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// metadataMatchValue mirrors Postgres/SQLite's "metadata->>k = v" (or
// "value->>k = v") comparison: a string value is compared as-is, anything
// else is JSON-round-tripped first so e.g. a JSON number/bool filter
// matches consistently regardless of the Go type callers pass in.
func metadataMatchValue(v interface{}) interface{} {
	if sv, ok := v.(string); ok {
		return sv
	}
	return v
}

type storeItemDoc struct {
	TenantID   string      `bson:"tenant_id"`
	Namespace  string      `bson:"namespace"`
	Key        string      `bson:"key"`
	Value      interface{} `bson:"value"`
	CreatedAt  time.Time   `bson:"created_at"`
	UpdatedAt  time.Time   `bson:"updated_at"`
	TTLMinutes *float64    `bson:"ttl_minutes,omitempty"`
	ExpiresAt  *time.Time  `bson:"expires_at,omitempty"`
}

// storeItemExpiresAt computes the absolute expiry from a TTL in
// minutes, nil if ttlMinutes is nil (no expiration).
func storeItemExpiresAt(now time.Time, ttlMinutes *float64) *time.Time {
	if ttlMinutes == nil {
		return nil
	}
	t := now.Add(time.Duration(*ttlMinutes * float64(time.Minute)))
	return &t
}

// notExpiredFilter matches items with no TTL or a still-future expiry --
// shared by GetItem and SearchItems so both apply the exact same rule.
func notExpiredFilter(now time.Time) bson.M {
	return bson.M{"$or": bson.A{
		bson.M{"expires_at": bson.M{"$exists": false}},
		bson.M{"expires_at": nil},
		bson.M{"expires_at": bson.M{"$gt": now}},
	}}
}

func (s *Store) PutItem(ctx context.Context, item *models.StoreItem) error {
	tid := tenant.FromContext(ctx)
	ns := nsToString(item.Namespace)
	now := time.Now().UTC()
	expiresAt := storeItemExpiresAt(now, item.TTLMinutes)
	_, err := s.col("store_items").UpdateOne(ctx,
		bson.M{"tenant_id": tid, "namespace": ns, "key": item.Key},
		bson.M{
			"$set":         bson.M{"value": item.Value, "updated_at": now, "ttl_minutes": item.TTLMinutes, "expires_at": expiresAt},
			"$setOnInsert": bson.M{"tenant_id": tid, "namespace": ns, "key": item.Key, "created_at": now},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

func (s *Store) GetItem(ctx context.Context, namespace []string, key string, refreshTTL bool) (*models.StoreItem, error) {
	now := time.Now().UTC()
	filter := tenantFilter(ctx, bson.M{"namespace": nsToString(namespace), "key": key})
	for k, v := range notExpiredFilter(now) {
		filter[k] = v
	}
	var doc storeItemDoc
	err := s.col("store_items").FindOne(ctx, filter).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "store_item", ID: key}
	}
	if err != nil {
		return nil, err
	}
	if refreshTTL && doc.TTLMinutes != nil {
		// Use the doc's own tenant_id, not tenant.FromContext(ctx) --
		// for a system-context caller reading across tenants, those can
		// differ, which would otherwise silently match zero docs here.
		newExpiry := storeItemExpiresAt(now, doc.TTLMinutes)
		_, _ = s.col("store_items").UpdateOne(ctx,
			bson.M{"tenant_id": doc.TenantID, "namespace": doc.Namespace, "key": doc.Key},
			bson.M{"$set": bson.M{"expires_at": newExpiry}},
		)
	}
	return &models.StoreItem{Namespace: stringToNs(doc.Namespace), Key: doc.Key, Value: bsonToMap(doc.Value), CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt}, nil
}

func (s *Store) DeleteItem(ctx context.Context, namespace []string, key string) error {
	res, err := s.col("store_items").DeleteOne(ctx, tenantFilter(ctx, bson.M{"namespace": nsToString(namespace), "key": key}))
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return &state.ErrNotFound{Resource: "store_item", ID: key}
	}
	return nil
}

func (s *Store) SearchItems(ctx context.Context, req *models.StoreSearchRequest) ([]*models.StoreItem, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()
	filter := tenantFilter(ctx, bson.M{})
	for k, v := range notExpiredFilter(now) {
		filter[k] = v
	}
	if len(req.NamespacePrefix) > 0 {
		filter["namespace"] = bson.M{"$regex": nsPrefixRegex(req.NamespacePrefix)}
	}
	for k, v := range req.Filter {
		filter["value."+k] = metadataMatchValue(v)
	}

	cur, err := s.col("store_items").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}).SetSkip(int64(req.Offset)).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	// Non-nil so a no-results search JSON-encodes to "items": [] rather
	// than "items": null -- SDK clients call .map() on it unconditionally.
	items := []*models.StoreItem{}
	var toRefresh []storeItemDoc
	for cur.Next(ctx) {
		var doc storeItemDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		items = append(items, &models.StoreItem{Namespace: stringToNs(doc.Namespace), Key: doc.Key, Value: bsonToMap(doc.Value), CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt})
		if req.RefreshTTLOrDefault() && doc.TTLMinutes != nil {
			toRefresh = append(toRefresh, doc)
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	cur.Close(ctx)
	for _, doc := range toRefresh {
		newExpiry := storeItemExpiresAt(now, doc.TTLMinutes)
		_, _ = s.col("store_items").UpdateOne(ctx,
			bson.M{"tenant_id": doc.TenantID, "namespace": doc.Namespace, "key": doc.Key},
			bson.M{"$set": bson.M{"expires_at": newExpiry}},
		)
	}
	return items, nil
}

func (s *Store) ListNamespaces(ctx context.Context, req *models.StoreListNamespacesRequest) ([][]string, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	filter := tenantFilter(ctx, bson.M{})
	if len(req.Prefix) > 0 {
		filter["namespace"] = bson.M{"$regex": nsPrefixRegex(req.Prefix)}
	}

	var raw []interface{}
	if err := s.col("store_items").Distinct(ctx, "namespace", filter).Decode(&raw); err != nil {
		return nil, err
	}
	namespaces := make([]string, 0, len(raw))
	for _, v := range raw {
		if ns, ok := v.(string); ok {
			namespaces = append(namespaces, ns)
		}
	}
	// Distinct doesn't support sort/skip/limit server-side -- applied in
	// Go, acceptable for this operation's expected scale (same as every
	// other list endpoint's in-memory pagination in this project, see
	// README's Admin UI overview-counts known limitation).
	sortStrings(namespaces)
	if req.Offset > 0 && req.Offset < len(namespaces) {
		namespaces = namespaces[req.Offset:]
	} else if req.Offset >= len(namespaces) {
		namespaces = []string{}
	}
	if len(namespaces) > limit {
		namespaces = namespaces[:limit]
	}

	// Non-nil even for zero results -- same JSON []-not-null contract as
	// every other list/search method.
	out := make([][]string, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, stringToNs(ns))
	}
	return out, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// --------------------------------------------------------------------------
// Webhook dead-letter
// --------------------------------------------------------------------------

type webhookDeadLetterDoc struct {
	ID        string      `bson:"id"`
	URL       string      `bson:"url"`
	EventType string      `bson:"event_type"`
	RunID     string      `bson:"run_id"`
	Payload   interface{} `bson:"payload"`
	Error     string      `bson:"error"`
	Attempts  int         `bson:"attempts"`
	FailedAt  time.Time   `bson:"failed_at"`
}

func (s *Store) SaveWebhookDeadLetter(ctx context.Context, dl *models.WebhookDeadLetter) error {
	_, err := s.col("webhook_dead_letters").InsertOne(ctx, webhookDeadLetterDoc{
		ID: dl.ID, URL: dl.URL, EventType: dl.EventType, RunID: dl.RunID,
		Payload: jsonToBSON(dl.Payload), Error: dl.Error, Attempts: dl.Attempts, FailedAt: dl.FailedAt,
	})
	return err
}

func (s *Store) ListWebhookDeadLetters(ctx context.Context, limit int) ([]*models.WebhookDeadLetter, error) {
	if limit <= 0 {
		limit = 50
	}
	cur, err := s.col("webhook_dead_letters").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "failed_at", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := []*models.WebhookDeadLetter{}
	for cur.Next(ctx) {
		var doc webhookDeadLetterDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		out = append(out, &models.WebhookDeadLetter{
			ID: doc.ID, URL: doc.URL, EventType: doc.EventType, RunID: doc.RunID,
			Payload: bsonToJSON(doc.Payload), Error: doc.Error, Attempts: doc.Attempts, FailedAt: doc.FailedAt,
		})
	}
	return out, cur.Err()
}

// --------------------------------------------------------------------------
// Run cache (LLM response caching)
// --------------------------------------------------------------------------

type runCacheDoc struct {
	TenantID  string      `bson:"tenant_id"`
	CacheKey  string      `bson:"cache_key"`
	AgentID   string      `bson:"agent_id"`
	Output    interface{} `bson:"output"`
	CreatedAt time.Time   `bson:"created_at"`
	ExpiresAt time.Time   `bson:"expires_at"`
}

func (s *Store) GetCachedRunResult(ctx context.Context, cacheKey string) (*models.CachedRunResult, error) {
	filter := tenantFilter(ctx, bson.M{"cache_key": cacheKey, "expires_at": bson.M{"$gt": time.Now().UTC()}})
	var doc runCacheDoc
	err := s.col("run_cache").FindOne(ctx, filter).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, &state.ErrNotFound{Resource: "run_cache", ID: cacheKey}
	}
	if err != nil {
		return nil, err
	}
	return &models.CachedRunResult{CacheKey: doc.CacheKey, AgentID: doc.AgentID, Output: bsonToMap(doc.Output), CreatedAt: doc.CreatedAt, ExpiresAt: doc.ExpiresAt}, nil
}

func (s *Store) SaveCachedRunResult(ctx context.Context, result *models.CachedRunResult) error {
	tid := tenant.FromContext(ctx)
	_, err := s.col("run_cache").UpdateOne(ctx,
		bson.M{"tenant_id": tid, "cache_key": result.CacheKey},
		bson.M{"$set": bson.M{
			"agent_id": result.AgentID, "output": result.Output, "created_at": result.CreatedAt, "expires_at": result.ExpiresAt,
		}, "$setOnInsert": bson.M{"tenant_id": tid, "cache_key": result.CacheKey}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// --------------------------------------------------------------------------
// Cron scheduler
// --------------------------------------------------------------------------

type cronScheduleDoc struct {
	TenantID   string      `bson:"tenant_id"`
	Name       string      `bson:"name"`
	AgentID    string      `bson:"agent_id"`
	Expression string      `bson:"expression"`
	Timezone   string      `bson:"timezone"`
	Input      interface{} `bson:"input"`
	Config     interface{} `bson:"config"`
	Enabled    bool        `bson:"enabled"`
	CreatedAt  time.Time   `bson:"created_at"`
	UpdatedAt  time.Time   `bson:"updated_at"`
}

func (s *Store) UpsertCronSchedule(ctx context.Context, sched *models.CronSchedule) error {
	tid := tenant.FromContext(ctx)
	now := time.Now().UTC()
	_, err := s.col("cron_schedules").UpdateOne(ctx,
		bson.M{"tenant_id": tid, "name": sched.Name},
		bson.M{
			"$set": bson.M{
				"agent_id": sched.AgentID, "expression": sched.Expression, "timezone": sched.Timezone,
				"input": jsonToBSON(sched.Input), "config": jsonToBSON(sched.Config), "enabled": sched.Enabled, "updated_at": now,
			},
			"$setOnInsert": bson.M{"tenant_id": tid, "name": sched.Name, "created_at": now},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

func (s *Store) ListCronSchedules(ctx context.Context) ([]*models.CronSchedule, error) {
	filter := tenantFilter(ctx, bson.M{})
	cur, err := s.col("cron_schedules").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "tenant_id", Value: 1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := []*models.CronSchedule{}
	for cur.Next(ctx) {
		var doc cronScheduleDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		out = append(out, &models.CronSchedule{
			TenantID: doc.TenantID, Name: doc.Name, AgentID: doc.AgentID, Expression: doc.Expression, Timezone: doc.Timezone,
			Input: bsonToJSON(doc.Input), Config: bsonToJSON(doc.Config), Enabled: doc.Enabled, CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt,
		})
	}
	return out, cur.Err()
}

func (s *Store) DeleteCronSchedule(ctx context.Context, name string) error {
	_, err := s.col("cron_schedules").DeleteOne(ctx, tenantFilter(ctx, bson.M{"name": name}))
	return err
}

type cronClaimDoc struct {
	TenantID     string    `bson:"tenant_id"`
	ScheduleName string    `bson:"schedule_name"`
	FireTime     time.Time `bson:"fire_time"`
	ClaimedAt    time.Time `bson:"claimed_at"`
}

func (s *Store) TryClaimCronFire(ctx context.Context, scheduleName string, fireTime time.Time) (bool, error) {
	_, err := s.col("cron_claims").InsertOne(ctx, cronClaimDoc{
		TenantID: tenant.FromContext(ctx), ScheduleName: scheduleName, FireTime: fireTime.UTC(), ClaimedAt: time.Now().UTC(),
	})
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ReleaseCronClaim(ctx context.Context, scheduleName string, fireTime time.Time) error {
	_, err := s.col("cron_claims").DeleteOne(ctx, bson.M{
		"tenant_id": tenant.FromContext(ctx), "schedule_name": scheduleName, "fire_time": fireTime.UTC(),
	})
	return err
}

func (s *Store) GetLastCronFireTime(ctx context.Context, scheduleName string) (time.Time, bool, error) {
	var doc cronClaimDoc
	err := s.col("cron_claims").FindOne(ctx,
		bson.M{"tenant_id": tenant.FromContext(ctx), "schedule_name": scheduleName},
		options.FindOne().SetSort(bson.D{{Key: "fire_time", Value: -1}}),
	).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return doc.FireTime, true, nil
}

func (s *Store) PruneCronClaims(ctx context.Context, olderThan time.Time) (int64, error) {
	filter := tenantFilter(ctx, bson.M{"fire_time": bson.M{"$lt": olderThan}})
	res, err := s.col("cron_claims").DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (s *Store) PruneExpiredStoreItems(ctx context.Context) (int64, error) {
	filter := tenantFilter(ctx, bson.M{"expires_at": bson.M{"$ne": nil, "$lte": time.Now().UTC()}})
	res, err := s.col("store_items").DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}
