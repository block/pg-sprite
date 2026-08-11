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
			kind: []executor.NativeStepKind{executor.NativeStepDirect, executor.NativeStepDirect},
		},
		{
			name: "foreign key validates separately",
			sql: []string{
				`ALTER TABLE app.orders ADD CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES app.customers (id) NOT VALID`,
				`ALTER TABLE app.orders VALIDATE CONSTRAINT orders_customer_fk`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepDirect, executor.NativeStepDirect},
		},
		{
			name: "primary key uses prebuilt unique index",
			sql: []string{
				`CREATE UNIQUE INDEX CONCURRENTLY orders_pkey ON app.orders (id)`,
				`ALTER TABLE app.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey`,
			},
			kind: []executor.NativeStepKind{executor.NativeStepConcurrentIndex, executor.NativeStepDirect},
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
		name string
		sql  []string
	}{
		{name: "empty", sql: nil},
		{name: "direct check scan", sql: []string{`ALTER TABLE app.orders ADD CONSTRAINT c CHECK (id > 0)`}},
		{name: "direct foreign key scan", sql: []string{`ALTER TABLE app.orders ADD CONSTRAINT fk FOREIGN KEY (customer_id) REFERENCES app.customers (id)`}},
		{name: "direct primary key build", sql: []string{`ALTER TABLE app.orders ADD PRIMARY KEY (id)`}},
		{name: "nonunique concurrent index cannot back primary key", sql: []string{`CREATE INDEX CONCURRENTLY orders_id ON app.orders (id)`}},
		{name: "volatile default", sql: []string{`ALTER TABLE app.orders ADD COLUMN created_at timestamptz DEFAULT now()`}},
		{name: "rewrite type change", sql: []string{`ALTER TABLE app.orders ALTER COLUMN id TYPE bigint`}},
		{name: "multiple operations", sql: []string{`ALTER TABLE app.orders ADD COLUMN a int, ADD COLUMN b int`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executor.PrepareNativeSequence(tt.sql)
			assert.ErrorIs(t, err, executor.ErrNotNativeSafe)
		})
	}
}
