package executor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

func TestPrepareNativeSequence(t *testing.T) {
	tests := []struct {
		name string
		sql  []string
		kind []executor.NativeStepKind
	}{
		{
			name: "check constraint validates separately",
			sql: []string{
				`ALTER TABLE "app"."orders" ADD CONSTRAINT "amount_positive" CHECK (amount > 0) NOT VALID`,
				`ALTER TABLE "app"."orders" VALIDATE CONSTRAINT "amount_positive"`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepDirect, executor.NativeStepValidateConstraint},
		},
		{
			name: "foreign key validates separately",
			sql: []string{
				`ALTER TABLE app.orders ADD CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES app.customers (id) NOT VALID`,
				`ALTER TABLE app.orders VALIDATE CONSTRAINT orders_customer_fk`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepDirect, executor.NativeStepValidateConstraint},
		},
		{
			name: "primary key uses prebuilt unique index",
			sql: []string{
				`CREATE UNIQUE INDEX CONCURRENTLY orders_pkey ON app.orders (id)`,
				`ALTER TABLE app.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepConcurrentIndex, executor.NativeStepUsingIndex},
		},
		{
			name: "unique constraint uses prebuilt unique index",
			sql: []string{
				`CREATE UNIQUE INDEX CONCURRENTLY orders_ref_key ON app.orders (ref)`,
				`ALTER TABLE app.orders ADD CONSTRAINT orders_ref_key UNIQUE USING INDEX orders_ref_key`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepConcurrentIndex, executor.NativeStepUsingIndex},
		},
		{
			name: "plain concurrent index",
			sql:  []string{`CREATE INDEX CONCURRENTLY orders_customer_idx ON app.orders (customer_id)`},
			kind: []executor.NativeStepKind{executor.NativeStepConcurrentIndex},
		},
		{
			name: "create table",
			sql:  []string{`CREATE TABLE app.orders (id int PRIMARY KEY)`},
			kind: []executor.NativeStepKind{executor.NativeStepDirect},
		},
		{
			name: "fast constant default",
			sql:  []string{`ALTER TABLE app.orders ADD COLUMN region text DEFAULT 'emea'`},
			kind: []executor.NativeStepKind{executor.NativeStepDirect},
		},
		{
			name: "metadata only",
			sql:  []string{`ALTER TABLE app.orders ALTER COLUMN region SET DEFAULT 'apac'`},
			kind: []executor.NativeStepKind{executor.NativeStepDirect},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps, err := executor.PrepareNativeSequence(tt.sql)
			require.NoError(t, err)
			require.Len(t, steps, len(tt.sql))
			for i := range steps {
				assert.Equal(t, tt.sql[i], steps[i].SQL())
				assert.Equal(t, tt.kind[i], steps[i].Kind())
			}
		})
	}
}

func TestPrepareNativeSequenceFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		sql     []string
		wantErr error
	}{
		{name: "empty", sql: nil, wantErr: executor.ErrNotNativeSafe},
		{name: "direct check scan", sql: []string{`ALTER TABLE app.orders ADD CONSTRAINT c CHECK (id > 0)`}, wantErr: executor.ErrNotNativeSafe},
		{name: "direct foreign key scan", sql: []string{`ALTER TABLE app.orders ADD CONSTRAINT fk FOREIGN KEY (customer_id) REFERENCES app.customers (id)`}, wantErr: executor.ErrNotNativeSafe},
		{name: "direct primary key build", sql: []string{`ALTER TABLE app.orders ADD PRIMARY KEY (id)`}, wantErr: executor.ErrNotNativeSafe},
		{name: "volatile default", sql: []string{`ALTER TABLE app.orders ADD COLUMN created_at timestamptz DEFAULT now()`}, wantErr: executor.ErrNotNativeSafe},
		{name: "stored generated column rewrites the table", sql: []string{`ALTER TABLE app.orders ADD COLUMN total numeric GENERATED ALWAYS AS (amount * 2) STORED`}, wantErr: executor.ErrNotNativeSafe},
		{name: "rewrite type change", sql: []string{`ALTER TABLE app.orders ALTER COLUMN id TYPE bigint`}, wantErr: executor.ErrNotNativeSafe},
		{name: "set not null scans without a proven check", sql: []string{`ALTER TABLE app.orders ALTER COLUMN id SET NOT NULL`}, wantErr: executor.ErrNotNativeSafe},
		{name: "multiple operations", sql: []string{`ALTER TABLE app.orders ADD COLUMN a int, ADD COLUMN b int`}, wantErr: executor.ErrNotNativeSafe},
		{name: "partition of locks the parent", sql: []string{`CREATE TABLE app.orders_2026 PARTITION OF app.orders FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`}, wantErr: executor.ErrNotNativeSafe},
		{name: "plain create index blocks writes", sql: []string{`CREATE INDEX orders_id ON app.orders (id)`}, wantErr: executor.ErrNotNativeSafe},
		{name: "unnamed concurrent index", sql: []string{`CREATE INDEX CONCURRENTLY ON app.orders (id)`}, wantErr: executor.ErrUnnamedIndex},
		{name: "if not exists concurrent index", sql: []string{`CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_id ON app.orders (id)`}, wantErr: executor.ErrIfNotExistsUnsupported},
		{name: "unqualified concurrent index table", sql: []string{`CREATE INDEX CONCURRENTLY orders_id ON orders (id)`}, wantErr: executor.ErrUnqualifiedTable},
		{name: "unqualified using index table", sql: []string{`ALTER TABLE orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey`}, wantErr: executor.ErrNotNativeSafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executor.PrepareNativeSequence(tt.sql)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

// A refusal names the failing statement's 1-based position so a caller can
// map it back to the sequence.
func TestPrepareNativeSequenceNamesTheFailingStep(t *testing.T) {
	_, err := executor.PrepareNativeSequence([]string{
		`ALTER TABLE app.orders ADD CONSTRAINT c CHECK (amount > 0) NOT VALID`,
		`ALTER TABLE app.orders ALTER COLUMN id TYPE bigint`,
	})
	require.ErrorIs(t, err, executor.ErrNotNativeSafe)
	assert.ErrorContains(t, err, "admit native step 2")
}
