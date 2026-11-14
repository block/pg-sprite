package migrate

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// withAck returns opts carrying value as acknowledgement a. It is written
// independently of ackKind.value, so a value that reads the wrong Options
// field fails the round trip below.
func withAck(t *testing.T, opts Options, a ackKind, value string) Options {
	t.Helper()
	switch a {
	case ackForce:
		opts.Force = value
	case ackAcceptBlocking:
		opts.AcceptBlocking = value
	case ackAllowDestructive:
		opts.AllowDestructive = value
	case ackRestart:
		opts.CopySwap.Restart = value
	case ackBlockWrites:
		opts.CopySwap.BlockWrites = value
	default:
		t.Fatalf("acknowledgement %q has no Options field in this test", a)
	}
	return opts
}

// setEveryField sets every field reachable from v to a non-zero value:
// "app.orders" for a string, true, 7, a new value behind a pointer, a
// function that returns zero values, and for an interface the value
// interfaceValues lists for its type. A field of a kind or interface it
// does not handle fails the test, so a new field is handled here rather
// than left zero.
func setEveryField(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	interfaceValues := map[reflect.Type]any{
		reflect.TypeFor[progress.Clock](): progress.WallClock{},
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			require.True(t, f.IsExported(), "%s.%s is unexported and cannot be set", path, f.Name)
			setEveryField(t, v.Field(i), path+"."+f.Name)
		}
	case reflect.String:
		v.SetString("app.orders")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Func:
		typ := v.Type()
		v.Set(reflect.MakeFunc(typ, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, typ.NumOut())
			for i := range out {
				out[i] = reflect.Zero(typ.Out(i))
			}
			return out
		}))
	case reflect.Interface:
		iv, ok := interfaceValues[v.Type()]
		require.True(t, ok, "%s has interface type %s, which setEveryField has no value for", path, v.Type())
		v.Set(reflect.ValueOf(iv))
	default:
		t.Fatalf("%s has kind %s, which setEveryField does not set", path, v.Kind())
	}
}

// assertSameFields asserts that every field reachable from got equals the
// one in want. A function is compared by whether it is set and by its code
// pointer, since Go defines no equality for set functions.
func assertSameFields(t *testing.T, want, got reflect.Value, path string) {
	t.Helper()
	switch want.Kind() {
	case reflect.Struct:
		for i := range want.NumField() {
			assertSameFields(t, want.Field(i), got.Field(i), path+"."+want.Type().Field(i).Name)
		}
	case reflect.Func:
		assert.Equal(t, want.IsNil(), got.IsNil(), "%s", path)
		assert.Equal(t, want.Pointer(), got.Pointer(), "%s", path)
	default:
		assert.Equal(t, want.Interface(), got.Interface(), "%s", path)
	}
}

// Acknowledgements names the registry's acknowledgements in RunDesired's
// rejection order, and a caller that changes the slice it was handed
// changes neither the registry nor the next call's answer.
func TestAcknowledgementsNamesTheRegistry(t *testing.T) {
	want := []string{"force", "accept-blocking", "allow-destructive", "restart", "block-writes"}
	got := Acknowledgements()
	require.Equal(t, want, got)
	for i, a := range ackKinds() {
		assert.Equal(t, string(a), got[i])
	}

	got[0] = "app.orders"
	got[1] = "restart"
	assert.Equal(t, want, Acknowledgements())
	assert.Equal(t, ackForce, ackKinds()[0])
}

// Options.Acknowledgements names exactly the acknowledgements a policy
// carries, so an adapter can tell the desired-state view set one aside.
func TestOptionsAcknowledgementsNamesWhatThePolicyCarries(t *testing.T) {
	t.Run("a default policy carries none", func(t *testing.T) {
		assert.Nil(t, DefaultOptions().Acknowledgements())
	})

	t.Run("each acknowledgement is named alone", func(t *testing.T) {
		for _, a := range ackKinds() {
			opts := withAck(t, DefaultOptions(), a, "app.orders")
			assert.Equal(t, []string{string(a)}, opts.Acknowledgements())
		}
	})

	t.Run("several are named in registry order", func(t *testing.T) {
		opts := DefaultOptions()
		opts.CopySwap.BlockWrites = "app.orders"
		opts.Force = "app.orders"
		opts.CopySwap.Restart = "app.orders"
		assert.Equal(t, []string{"force", "restart", "block-writes"}, opts.Acknowledgements())
	})

	t.Run("the desired-state view carries none and the original keeps its own", func(t *testing.T) {
		opts := DefaultOptions()
		opts.AllowDestructive = "app.orders"
		assert.Nil(t, opts.WithoutAcknowledgements().Acknowledgements())
		assert.Equal(t, []string{"allow-destructive"}, opts.Acknowledgements())
	})

	t.Run("changing the returned slice changes nothing", func(t *testing.T) {
		opts := DefaultOptions()
		opts.AcceptBlocking = "app.orders"
		got := opts.Acknowledgements()
		got[0] = "force"
		assert.Equal(t, []string{"accept-blocking"}, opts.Acknowledgements())
		assert.Equal(t, "app.orders", opts.AcceptBlocking)
		assert.Empty(t, opts.Force)
		assert.Equal(t, "accept-blocking", Acknowledgements()[1])
	})
}

// The registry lists every irreversible operation with the acknowledgement
// that lifts its refusal, and desired-state execution takes none of those
// acknowledgements. A new acknowledgement, or a new shape the planner marks
// destructive, fails here until it has an entry and a desired-state
// decision.
func TestIrreversibleOperationRegistryIsComplete(t *testing.T) {
	acks := ackKinds()
	ops := irreversibleOps()
	destructive := planner.DestructiveOpKinds()

	t.Run("the walk is not vacuous", func(t *testing.T) {
		require.NotEmpty(t, destructive)
		require.Contains(t, destructive, statement.OpDropColumn)
		assert.Len(t, acks, 5)
		keys := make([]string, 0, len(ops))
		for _, op := range ops {
			keys = append(keys, op.key)
		}
		assert.Contains(t, keys, "copy-and-swap restart")
		assert.Len(t, ops, len(destructive)+5)
	})

	t.Run("every acknowledgement reads its own field and has its own sentinel", func(t *testing.T) {
		sentinels := map[error]ackKind{}
		for _, a := range acks {
			got, ok := a.value(withAck(t, DefaultOptions(), a, "app.orders"))
			require.True(t, ok, "acknowledgement %q is not classified", a)
			assert.Equal(t, "app.orders", got, "acknowledgement %q reads another field", a)
			for _, other := range acks {
				if other != a {
					v, _ := other.value(withAck(t, DefaultOptions(), a, "app.orders"))
					assert.Empty(t, v, "acknowledgement %q also reads %q's field", other, a)
				}
			}
			err := a.desiredErr()
			require.Error(t, err, "acknowledgement %q has no desired-state sentinel", a)
			prev, dup := sentinels[err]
			assert.False(t, dup, "acknowledgements %q and %q share a sentinel", prev, a)
			sentinels[err] = a
		}
	})

	// Each acknowledgement accepts a loss for one statement the operator
	// wrote; desired-state execution runs statements the plan derived, so
	// it rejects every one before any database work, even when it names
	// the right table.
	t.Run("desired-state execution rejects every acknowledgement", func(t *testing.T) {
		for _, a := range acks {
			opts := withAck(t, DefaultOptions(), a, "public.t")
			res, err := RunDesired(t.Context(), nil, DesiredRequest{Schema: "public"}, opts)
			require.ErrorIs(t, err, a.desiredErr(), "acknowledgement %q", a)
			assert.Equal(t, DesiredResult{}, res)
		}
	})

	// The desired-state view clears every acknowledgement the registry
	// walks and keeps every other field. Every field of the policy is set
	// to a non-zero value, including fields added later, so a view that
	// blanks a policy field such as KeepOld or the copy connection fails
	// here rather than reaching an adapter as a changed run.
	t.Run("the desired-state view clears every acknowledgement and nothing else", func(t *testing.T) {
		var shared Options
		setEveryField(t, reflect.ValueOf(&shared).Elem(), "Options")
		want := shared
		for _, a := range acks {
			want = withAck(t, want, a, "")
		}

		view := shared.WithoutAcknowledgements()
		assertSameFields(t, reflect.ValueOf(want), reflect.ValueOf(view), "Options")
		require.NoError(t, rejectDesiredAcks(view))
		for _, a := range acks {
			v, _ := a.value(shared)
			assert.Equal(t, "app.orders", v, "the view modified the original's %q", a)
		}
	})

	// Two acknowledgements left set together are also an unrunnable pair on
	// the imperative front door; desired-state execution still names the
	// first one's sentinel, so an adapter keying on the sentinels sees it.
	t.Run("desired-state execution names the first of several acknowledgements", func(t *testing.T) {
		opts := DefaultOptions()
		opts.Force = "public.t"
		opts.CopySwap.Restart = "public.t"
		_, err := RunDesired(t.Context(), nil, DesiredRequest{Schema: "public"}, opts)
		require.ErrorIs(t, err, ErrForceNotSupported)
	})

	// Every string or bool field on the policy either carries one of the
	// acknowledgements or is listed here as not one. A string field is an
	// acknowledgement when one of them reads it; a bool cannot name its
	// table, so it is never one and must be listed. A new acknowledgement
	// field, or a switch that opens a new irreversible step, added without
	// a registry entry would otherwise reach every statement a
	// desired-state plan derives.
	t.Run("every string or bool field on the policy is an acknowledgement or declared not one", func(t *testing.T) {
		notAcks := map[string]bool{
			// The role a greenfield table is created as: who owns the result.
			"Options.CreateOwner": true,
			// Opens the copy-and-swap route; each irreversible step on it
			// has its own acknowledgement.
			"CopySwapOptions.Enabled": true,
			// Keeps the retained source, so it loses less, not more.
			"CopySwapOptions.KeepOld": true,
		}
		var unaccounted, walked []string
		check := func(typ reflect.Type, field func(opts *Options) reflect.Value) {
			for f := range typ.Fields() {
				kind := f.Type.Kind()
				if kind != reflect.String && kind != reflect.Bool {
					continue
				}
				name := typ.Name() + "." + f.Name
				walked = append(walked, name)
				opts := DefaultOptions()
				if kind == reflect.Bool {
					field(&opts).FieldByName(f.Name).SetBool(true)
				} else {
					field(&opts).FieldByName(f.Name).SetString("app.orders")
				}
				if notAcks[name] {
					assert.Empty(t, opts.Acknowledgements(), "%s is declared not an acknowledgement", name)
					continue
				}
				if kind == reflect.Bool || len(opts.Acknowledgements()) != 1 {
					unaccounted = append(unaccounted, name)
				}
			}
		}
		check(reflect.TypeFor[Options](), func(opts *Options) reflect.Value {
			return reflect.ValueOf(opts).Elem()
		})
		check(reflect.TypeFor[CopySwapOptions](), func(opts *Options) reflect.Value {
			return reflect.ValueOf(&opts.CopySwap).Elem()
		})
		assert.Empty(t, unaccounted, "string or bool fields that are neither an acknowledgement nor declared not one")
		for name := range notAcks {
			assert.Contains(t, walked, name, "declared not an acknowledgement but no such string or bool field")
		}
	})

	// The hand-listed entries name the acknowledgement and refusal their
	// real gate uses, not merely members of the closed sets.
	t.Run("each listed entry names its gate's acknowledgement and refusal", func(t *testing.T) {
		want := map[string]struct {
			ack     ackKind
			refusal verdict.Reason
		}{
			"blocking DROP INDEX":                           {ackAcceptBlocking, verdict.ReasonIndexStatement},
			"blocking REINDEX":                              {ackAcceptBlocking, verdict.ReasonIndexStatement},
			"submitted form that blocks the table":          {ackForce, verdict.ReasonRewriteRequired},
			"copy-and-swap restart":                         {ackRestart, ""},
			"copy-and-swap with the table's writes blocked": {ackBlockWrites, verdict.ReasonCopyAndSwapRefused},
		}
		seen := 0
		for _, op := range ops {
			if op.desired == desiredRefusesPlan {
				assert.Equal(t, ackAllowDestructive, op.ack, "entry %q", op.key)
				assert.Equal(t, verdict.ReasonDestructiveChange, op.refusal, "entry %q", op.key)
				continue
			}
			w, ok := want[op.key]
			if !assert.True(t, ok, "entry %q has no expected gate", op.key) {
				continue
			}
			seen++
			assert.Equal(t, w.ack, op.ack, "entry %q", op.key)
			assert.Equal(t, w.refusal, op.refusal, "entry %q", op.key)
		}
		assert.Equal(t, len(want), seen)
	})

	t.Run("every destructive shape has exactly one entry", func(t *testing.T) {
		var shapes []string
		for _, op := range ops {
			if op.desired == desiredRefusesPlan {
				shapes = append(shapes, op.key)
			}
		}
		want := make([]string, 0, len(destructive))
		for _, k := range destructive {
			want = append(want, "shape: "+statement.Op{Kind: k}.Describe())
		}
		assert.Equal(t, want, shapes)
	})

	t.Run("every entry is gated and every acknowledgement gates an entry", func(t *testing.T) {
		gated := map[ackKind]bool{}
		for _, op := range ops {
			assert.Contains(t, acks, op.ack, "entry %q", op.key)
			gated[op.ack] = true
			assert.Contains(t, []loss{lossStructure, lossAvailability, lossProgress}, op.loss, "entry %q", op.key)
			assert.Contains(t, []desiredHandling{desiredRefusesPlan, desiredRejectsAck}, op.desired, "entry %q", op.key)
			if op.loss == lossProgress {
				assert.Empty(t, op.refusal, "entry %q: without the acknowledgement the run resumes", op.key)
				continue
			}
			assert.Contains(t, verdict.Reasons(), op.refusal, "entry %q", op.key)
		}
		for _, a := range acks {
			assert.True(t, gated[a], "acknowledgement %q gates no entry", a)
		}
	})

	// The shapes desired-state execution can derive are refused at plan
	// admission, whole, before any statement runs.
	t.Run("desired-state admission refuses a destructive plan whole", func(t *testing.T) {
		report := plan.Report{
			Schema: "app",
			Table:  "orders",
			Statements: []plan.Statement{
				{SQL: `ALTER TABLE app.orders ADD COLUMN note text`},
				{SQL: `ALTER TABLE app.orders DROP COLUMN legacy_id`, Destructive: true},
			},
		}
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		for _, op := range ops {
			if op.desired == desiredRefusesPlan {
				assert.Equal(t, verdict.ReasonDestructiveChange, op.refusal, "entry %q", op.key)
			}
		}
	})
}
