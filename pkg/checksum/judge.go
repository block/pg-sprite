package checksum

// verdict is what two reads of one key say together.
type verdict int

const (
	// verdictEqual is a reread whose two sides agree on the key.
	verdictEqual verdict = iota
	// verdictSettled is a reread whose shadow holds the key as the earlier
	// read saw it on the source: the shadow has reached the state the
	// source was read in, and the source has moved on since, which the
	// stream carries.
	verdictSettled
	// verdictHot is a reread where the source or the shadow changed the key
	// since the earlier read and the two sides still differ: nothing can be
	// concluded, and the key is read again.
	verdictHot
	// verdictDiverged is a reread where neither side changed the key since
	// the earlier read, the catch-up confirmed past that read, and the two
	// sides still differ: the stream has delivered everything the source
	// holds for the key and the shadow does not match it.
	verdictDiverged
)

// judge compares one key's reread with the read before it. The reread was
// taken after the catch-up confirmed past the earlier read's position. A
// side that holds no row for the key reads as the zero hash, so a key
// inserted on the source since the earlier read is settled — the shadow
// holds what the earlier read saw, nothing, and the stream carries the
// insert — and a key deleted on both sides is equal.
func judge(prior, now keyPair) verdict {
	if now.shadow == now.source {
		return verdictEqual
	}
	if now.shadow == prior.source {
		return verdictSettled
	}
	if now.source != prior.source {
		return verdictHot
	}
	if now.shadow != prior.shadow {
		return verdictHot
	}
	return verdictDiverged
}

// keyPair is one key as one read saw it on both sides.
type keyPair struct {
	source rowHash
	shadow rowHash
}

// pair returns how the observation saw key on both sides.
func (o observation) pair(key int64) keyPair {
	return keyPair{source: o.source.at(key), shadow: o.shadow.at(key)}
}

// judgeKeys judges every key in keys between the two reads and returns the
// keys still hot, in the order given, and how many diverged. A chunk is
// judged from the keys that stayed hot at its previous reread, or from
// every key either read holds on either side when this is its first.
func judgeKeys(prior, now observation, keys []int64) (hot []int64, diverged int) {
	for _, key := range keys {
		switch judge(prior.pair(key), now.pair(key)) {
		case verdictHot:
			hot = append(hot, key)
		case verdictDiverged:
			diverged++
		case verdictEqual, verdictSettled:
		}
	}
	return hot, diverged
}
