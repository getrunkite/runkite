package migrate

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const mongoMigrationsCollection = "schema_migrations"

// Mongo is a Bookkeeper backed by a MongoDB database.
type Mongo struct {
	DB *mongo.Database
}

// NewMongo returns a Bookkeeper for MongoDB.
func NewMongo(db *mongo.Database) *Mongo {
	return &Mongo{DB: db}
}

func (m *Mongo) col() *mongo.Collection {
	return m.DB.Collection(mongoMigrationsCollection)
}

func (m *Mongo) Ensure(ctx context.Context) error {
	_, err := m.col().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "version", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

func (m *Mongo) Current(ctx context.Context) (int, error) {
	opts := options.FindOne().SetSort(bson.D{{Key: "version", Value: -1}})
	var doc struct {
		Version int `bson:"version"`
	}
	err := m.col().FindOne(ctx, bson.M{}, opts).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return doc.Version, nil
}

func (m *Mongo) Insert(ctx context.Context, version int, name string) error {
	_, err := m.col().InsertOne(ctx, bson.M{
		"version":    version,
		"name":       name,
		"applied_at": time.Now().UTC(),
	})
	return err
}

func (m *Mongo) Delete(ctx context.Context, version int) error {
	res, err := m.col().DeleteOne(ctx, bson.M{"version": version})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fmt.Errorf("schema_migrations: version %d not found", version)
	}
	return nil
}

// MongoCollectionExists reports whether a collection name is present.
func MongoCollectionExists(ctx context.Context, db *mongo.Database, name string) (bool, error) {
	names, err := db.ListCollectionNames(ctx, bson.M{"name": name})
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}
