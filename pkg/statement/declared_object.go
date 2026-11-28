package statement

import (
	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// DeclaredObject is the kind of database object a statement declares,
// changes, or annotates. It is finer than [Kind] where a refusal of the
// statement from a desired file needs it: a comment, a trigger, and a domain
// all fall under one Kind, yet each has a different place it belongs instead,
// and the file's author needs that place.
type DeclaredObject int

// The objects a refused desired-file statement is told apart by. Every
// statement that is none of them is DeclaredOther.
const (
	DeclaredOther DeclaredObject = iota
	// DeclaredComment is COMMENT ON, on any object.
	DeclaredComment
	// DeclaredTrigger is CREATE TRIGGER or CREATE EVENT TRIGGER.
	DeclaredTrigger
	// DeclaredFunction is CREATE FUNCTION or CREATE PROCEDURE.
	DeclaredFunction
	// DeclaredView is CREATE VIEW.
	DeclaredView
	// DeclaredMaterializedView is CREATE MATERIALIZED VIEW.
	DeclaredMaterializedView
	// DeclaredDomain is CREATE DOMAIN.
	DeclaredDomain
	// DeclaredStatistics is CREATE STATISTICS.
	DeclaredStatistics
	// DeclaredSequence is CREATE SEQUENCE.
	DeclaredSequence
	// DeclaredType is CREATE TYPE other than an enum: a composite, range,
	// or base type.
	DeclaredType
	// DeclaredExtension is CREATE EXTENSION.
	DeclaredExtension
	// DeclaredGrant is GRANT or REVOKE, of privileges or of a role.
	DeclaredGrant
	// DeclaredSchema is CREATE SCHEMA.
	DeclaredSchema
	// DeclaredRule is CREATE RULE.
	DeclaredRule
	// DeclaredEnumType is CREATE TYPE ... AS ENUM. A desired file may hold
	// one, but only alone, so beside a table it belongs in a file of its own.
	DeclaredEnumType
	// DeclaredEnumLabel is ALTER TYPE ... ADD VALUE or RENAME VALUE: a
	// change to an enum type's labels, which a desired enum type file
	// declares in its CREATE TYPE instead.
	DeclaredEnumLabel
	// DeclaredTable is CREATE TABLE, other than CREATE TABLE ... AS.
	DeclaredTable
	// DeclaredIndex is CREATE INDEX.
	DeclaredIndex
	// DeclaredPolicy is CREATE POLICY.
	DeclaredPolicy
	// DeclaredRowSecurity is an ALTER TABLE that only enables, disables,
	// forces, or stops forcing row security.
	DeclaredRowSecurity
)

// DeclaredObjects returns the closed set of declared objects,
// DeclaredOther first, so a caller that maps each to its own wording can
// prove it covers them all.
func DeclaredObjects() []DeclaredObject {
	return []DeclaredObject{
		DeclaredOther,
		DeclaredComment,
		DeclaredTrigger,
		DeclaredFunction,
		DeclaredView,
		DeclaredMaterializedView,
		DeclaredDomain,
		DeclaredStatistics,
		DeclaredSequence,
		DeclaredType,
		DeclaredExtension,
		DeclaredGrant,
		DeclaredSchema,
		DeclaredRule,
		DeclaredEnumType,
		DeclaredEnumLabel,
		DeclaredTable,
		DeclaredIndex,
		DeclaredPolicy,
		DeclaredRowSecurity,
	}
}

// String names the object in lower case, as a noun a message can use.
func (o DeclaredObject) String() string {
	switch o {
	case DeclaredComment:
		return "comment"
	case DeclaredTrigger:
		return "trigger"
	case DeclaredFunction:
		return "function"
	case DeclaredView:
		return "view"
	case DeclaredMaterializedView:
		return "materialized view"
	case DeclaredDomain:
		return "domain"
	case DeclaredStatistics:
		return "extended statistics object"
	case DeclaredSequence:
		return "sequence"
	case DeclaredType:
		return "type"
	case DeclaredExtension:
		return "extension"
	case DeclaredGrant:
		return "privilege grant"
	case DeclaredSchema:
		return "schema"
	case DeclaredRule:
		return "rule"
	case DeclaredEnumType:
		return "enum type"
	case DeclaredEnumLabel:
		return "enum label change"
	case DeclaredTable:
		return "table"
	case DeclaredIndex:
		return "index"
	case DeclaredPolicy:
		return "policy"
	case DeclaredRowSecurity:
		return "row security change"
	default:
		return "statement"
	}
}

// declaredObject reads the object node declares from its parse tree.
func declaredObject(node *pganalyze.Node) DeclaredObject {
	switch {
	case node.GetCommentStmt() != nil:
		return DeclaredComment
	case node.GetCreateTrigStmt() != nil, node.GetCreateEventTrigStmt() != nil:
		return DeclaredTrigger
	case node.GetCreateFunctionStmt() != nil:
		return DeclaredFunction
	case node.GetViewStmt() != nil:
		return DeclaredView
	case node.GetCreateTableAsStmt().GetObjtype() == pganalyze.ObjectType_OBJECT_MATVIEW:
		return DeclaredMaterializedView
	case node.GetCreateDomainStmt() != nil:
		return DeclaredDomain
	case node.GetCreateStatsStmt() != nil:
		return DeclaredStatistics
	case node.GetCreateSeqStmt() != nil:
		return DeclaredSequence
	case node.GetCompositeTypeStmt() != nil, node.GetCreateRangeStmt() != nil,
		node.GetDefineStmt().GetKind() == pganalyze.ObjectType_OBJECT_TYPE:
		return DeclaredType
	case node.GetCreateExtensionStmt() != nil:
		return DeclaredExtension
	case node.GetGrantStmt() != nil, node.GetGrantRoleStmt() != nil:
		return DeclaredGrant
	case node.GetCreateSchemaStmt() != nil:
		return DeclaredSchema
	case node.GetRuleStmt() != nil:
		return DeclaredRule
	case node.GetCreateEnumStmt() != nil:
		return DeclaredEnumType
	case node.GetAlterEnumStmt() != nil:
		return DeclaredEnumLabel
	case node.GetCreateStmt() != nil:
		return DeclaredTable
	case node.GetIndexStmt() != nil:
		return DeclaredIndex
	case node.GetCreatePolicyStmt() != nil:
		return DeclaredPolicy
	case node.GetAlterTableStmt() != nil && changesOnlyRowSecurity(node.GetAlterTableStmt()):
		return DeclaredRowSecurity
	default:
		return DeclaredOther
	}
}
