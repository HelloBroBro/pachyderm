package clusterstate

import (
	"context"

	col "github.com/pachyderm/pachyderm/v2/src/internal/collection"
	"github.com/pachyderm/pachyderm/v2/src/internal/migrations"
	"github.com/pachyderm/pachyderm/v2/src/internal/pfsdb"
)

var state_2_4_0 migrations.State = state_2_3_0.
	Apply("Add projects collection", func(ctx context.Context, env migrations.Env) error {
		return col.SetupPostgresCollections(ctx, env.Tx, pfsdb.CollectionsV2_4_0()...)
	})
	// DO NOT MODIFY THIS STATE
	// IT HAS ALREADY SHIPPED IN A RELEASE