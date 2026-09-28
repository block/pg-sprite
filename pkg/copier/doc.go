// Package copier cuts a table's primary-key space into row-count chunks for
// the shadow-table copy, tiling the whole int64 range so every key belongs to
// exactly one chunk (CO-4) and copy work stays bounded per statement (LK-3).
package copier
