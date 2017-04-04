package tsm1

// Compactions are the process of creating read-optimized TSM files.
// The files are created by converting write-optimized WAL entries
// to read-optimized TSM format.  They can also be created from existing
// TSM files when there are tombstone records that neeed to be removed, points
// that were overwritten by later writes and need to updated, or multiple
// smaller TSM files need to be merged to reduce file counts and improve
// compression ratios.
//
// The compaction process is stream-oriented using multiple readers and
// iterators.  The resulting stream is written sorted and chunked to allow for
// one-pass writing of a new TSM file.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/tsdb"
)

const maxTSMFileSize = uint32(2048 * 1024 * 1024) // 2GB

const (
	// CompactionTempExtension is the extension used for temporary files created during compaction.
	CompactionTempExtension = "tmp"

	// TSMFileExtension is the extension used for TSM files.
	TSMFileExtension = "tsm"
)

var (
	errMaxFileExceeded      = fmt.Errorf("max file exceeded")
	errSnapshotsDisabled    = fmt.Errorf("snapshots disabled")
	errCompactionsDisabled  = fmt.Errorf("compactions disabled")
	errCompactionAborted    = fmt.Errorf("compaction aborted")
	errCompactionInProgress = fmt.Errorf("compaction in progress")
)

// CompactionGroup represents a list of files eligible to be compacted together.
type CompactionGroup []string

// CompactionPlanner determines what TSM files and WAL segments to include in a
// given compaction run.
type CompactionPlanner interface {
	Plan(lastWrite time.Time) []CompactionGroup
	PlanLevel(level int) []CompactionGroup
	PlanOptimize() []CompactionGroup
}

// DefaultPlanner implements CompactionPlanner using a strategy to roll up
// multiple generations of TSM files into larger files in stages.  It attempts
// to minimize the number of TSM files on disk while rolling up a bounder number
// of files.
type DefaultPlanner struct {
	FileStore interface {
		Stats() []FileStat
		LastModified() time.Time
		BlockCount(path string, idx int) int
	}

	// CompactFullWriteColdDuration specifies the length of time after
	// which if no writes have been committed to the WAL, the engine will
	// do a full compaction of the TSM files in this shard. This duration
	// should always be greater than the CacheFlushWriteColdDuraion
	CompactFullWriteColdDuration time.Duration

	// lastPlanCheck is the last time Plan was called
	lastPlanCheck time.Time

	mu sync.RWMutex
	// lastFindGenerations is the last time findGenerations was run
	lastFindGenerations time.Time

	// lastGenerations is the last set of generations found by findGenerations
	lastGenerations tsmGenerations
}

// tsmGeneration represents the TSM files within a generation.
// 000001-01.tsm, 000001-02.tsm would be in the same generation
// 000001 each with different sequence numbers.
type tsmGeneration struct {
	id    int
	files []FileStat
}

// size returns the total size of the files in the generation.
func (t *tsmGeneration) size() uint64 {
	var n uint64
	for _, f := range t.files {
		n += uint64(f.Size)
	}
	return n
}

// compactionLevel returns the level of the files in this generation.
func (t *tsmGeneration) level() int {
	// Level 0 is always created from the result of a cache compaction.  It generates
	// 1 file with a sequence num of 1.  Level 2 is generated by compacting multiple
	// level 1 files.  Level 3 is generate by compacting multiple level 2 files.  Level
	// 4 is for anything else.
	_, seq, _ := ParseTSMFileName(t.files[0].Path)
	if seq < 4 {
		return seq
	}

	return 4
}

func (t *tsmGeneration) lastModified() int64 {
	var max int64
	for _, f := range t.files {
		if f.LastModified > max {
			max = f.LastModified
		}
	}
	return max
}

// count returns the number of files in the generation.
func (t *tsmGeneration) count() int {
	return len(t.files)
}

// hasTombstones returns true if there are keys removed for any of the files.
func (t *tsmGeneration) hasTombstones() bool {
	for _, f := range t.files {
		if f.HasTombstone {
			return true
		}
	}
	return false
}

// PlanLevel returns a set of TSM files to rewrite for a specific level.
func (c *DefaultPlanner) PlanLevel(level int) []CompactionGroup {
	// Determine the generations from all files on disk.  We need to treat
	// a generation conceptually as a single file even though it may be
	// split across several files in sequence.
	generations := c.findGenerations()

	// If there is only one generation and no tombstones, then there's nothing to
	// do.
	if len(generations) <= 1 && !generations.hasTombstones() {
		return nil
	}

	// Group each generation by level such that two adjacent generations in the same
	// level become part of the same group.
	var currentGen tsmGenerations
	var groups []tsmGenerations
	for i := 0; i < len(generations); i++ {
		cur := generations[i]

		if len(currentGen) == 0 || currentGen[0].level() == cur.level() {
			currentGen = append(currentGen, cur)
			continue
		}
		groups = append(groups, currentGen)

		currentGen = tsmGenerations{}
		currentGen = append(currentGen, cur)
	}

	if len(currentGen) > 0 {
		groups = append(groups, currentGen)
	}

	// Remove any groups in the wrong level
	var levelGroups []tsmGenerations
	for _, cur := range groups {
		if cur[0].level() == level {
			levelGroups = append(levelGroups, cur)
		}
	}

	// Determine the minimum number of files required for the level.  Higher levels are more
	// CPU intensive so we only want to include them when we have enough data to make them
	// worthwhile.
	// minGenerations 1 -> 2
	// minGenerations 2 -> 2
	// minGenerations 3 -> 4
	// minGenerations 4 -> 4
	minGenerations := level
	if minGenerations%2 != 0 {
		minGenerations = level + 1
	}

	var cGroups []CompactionGroup
	for _, group := range levelGroups {
		for _, chunk := range group.chunk(4) {
			var cGroup CompactionGroup
			var hasTombstones bool
			for _, gen := range chunk {
				if gen.hasTombstones() {
					hasTombstones = true
				}
				for _, file := range gen.files {
					cGroup = append(cGroup, file.Path)
				}
			}

			if len(chunk) < minGenerations && !hasTombstones {
				continue
			}

			cGroups = append(cGroups, cGroup)
		}
	}

	return cGroups
}

// PlanOptimize returns all TSM files if they are in different generations in order
// to optimize the index across TSM files.  Each returned compaction group can be
// compacted concurrently.
func (c *DefaultPlanner) PlanOptimize() []CompactionGroup {
	// Determine the generations from all files on disk.  We need to treat
	// a generation conceptually as a single file even though it may be
	// split across several files in sequence.
	generations := c.findGenerations()

	// If there is only one generation and no tombstones, then there's nothing to
	// do.
	if len(generations) <= 1 && !generations.hasTombstones() {
		return nil
	}

	// Group each generation by level such that two adjacent generations in the same
	// level become part of the same group.
	var currentGen tsmGenerations
	var groups []tsmGenerations
	for i := 0; i < len(generations); i++ {
		cur := generations[i]

		if len(currentGen) == 0 || currentGen[0].level() == cur.level() {
			currentGen = append(currentGen, cur)
			continue
		}
		groups = append(groups, currentGen)

		currentGen = tsmGenerations{}
		currentGen = append(currentGen, cur)
	}

	if len(currentGen) > 0 {
		groups = append(groups, currentGen)
	}

	// Only optimize level 4 files since using lower-levels will collide
	// with the level planners
	var levelGroups []tsmGenerations
	for _, cur := range groups {
		if cur[0].level() == 4 {
			levelGroups = append(levelGroups, cur)
		}
	}

	var cGroups []CompactionGroup
	for _, group := range levelGroups {
		// Skip the group if it's not worthwhile to optimize it
		if len(group) < 4 && !group.hasTombstones() {
			continue
		}

		var cGroup CompactionGroup
		for _, gen := range group {
			for _, file := range gen.files {
				cGroup = append(cGroup, file.Path)
			}
		}

		cGroups = append(cGroups, cGroup)
	}

	return cGroups
}

// Plan returns a set of TSM files to rewrite for level 4 or higher.  The planning returns
// multiple groups if possible to allow compactions to run concurrently.
func (c *DefaultPlanner) Plan(lastWrite time.Time) []CompactionGroup {
	generations := c.findGenerations()

	// first check if we should be doing a full compaction because nothing has been written in a long time
	if c.CompactFullWriteColdDuration > 0 && time.Since(lastWrite) > c.CompactFullWriteColdDuration && len(generations) > 1 {
		var tsmFiles []string
		var genCount int
		for i, group := range generations {
			var skip bool

			// Skip the file if it's over the max size and contains a full block and it does not have any tombstones
			if len(generations) > 2 && group.size() > uint64(maxTSMFileSize) && c.FileStore.BlockCount(group.files[0].Path, 1) == tsdb.DefaultMaxPointsPerBlock && !group.hasTombstones() {
				skip = true
			}

			// We need to look at the level of the next file because it may need to be combined with this generation
			// but won't get picked up on it's own if this generation is skipped.  This allows the most recently
			// created files to get picked up by the full compaction planner and avoids having a few less optimally
			// compressed files.
			if i < len(generations)-1 {
				if generations[i+1].level() <= 3 {
					skip = false
				}
			}

			if skip {
				continue
			}

			for _, f := range group.files {
				tsmFiles = append(tsmFiles, f.Path)
			}
			genCount += 1
		}
		sort.Strings(tsmFiles)

		// Make sure we have more than 1 file and more than 1 generation
		if len(tsmFiles) <= 1 || genCount <= 1 {
			return nil
		}

		return []CompactionGroup{tsmFiles}
	}

	// don't plan if nothing has changed in the filestore
	if c.lastPlanCheck.After(c.FileStore.LastModified()) && !generations.hasTombstones() {
		return nil
	}

	c.lastPlanCheck = time.Now()

	// If there is only one generation, return early to avoid re-compacting the same file
	// over and over again.
	if len(generations) <= 1 && !generations.hasTombstones() {
		return nil
	}

	// Need to find the ending point for level 4 files.  They will be the oldest files. We scan
	// each generation in descending break once we see a file less than 4.
	end := 0
	start := 0
	for i, g := range generations {
		if g.level() <= 3 {
			break
		}
		end = i + 1
	}

	// As compactions run, the oldest files get bigger.  We don't want to re-compact them during
	// this planning if they are maxed out so skip over any we see.
	var hasTombstones bool
	for i, g := range generations[:end] {
		if g.hasTombstones() {
			hasTombstones = true
		}

		if hasTombstones {
			continue
		}

		// Skip the file if it's over the max size and contains a full block or the generation is split
		// over multiple files.  In the latter case, that would mean the data in the file spilled over
		// the 2GB limit.
		if g.size() > uint64(maxTSMFileSize) && c.FileStore.BlockCount(g.files[0].Path, 1) == tsdb.DefaultMaxPointsPerBlock || g.count() > 1 {
			start = i + 1
		}

		// This is an edge case that can happen after multiple compactions run.  The files at the beginning
		// can become larger faster than ones after them.  We want to skip those really big ones and just
		// compact the smaller ones until they are closer in size.
		if i > 0 {
			if g.size()*2 < generations[i-1].size() {
				start = i
				break
			}
		}
	}

	// step is how may files to compact in a group.  We want to clamp it at 4 but also stil
	// return groups smaller than 4.
	step := 4
	if step > end {
		step = end
	}

	// slice off the generations that we'll examine
	generations = generations[start:end]

	// Loop through the generations in groups of size step and see if we can compact all (or
	// some of them as group)
	groups := []tsmGenerations{}
	for i := 0; i < len(generations); i += step {
		var skipGroup bool
		startIndex := i

		for j := i; j < i+step && j < len(generations); j++ {
			gen := generations[j]
			lvl := gen.level()

			// Skip compacting this group if there happens to be any lower level files in the
			// middle.  These will get picked up by the level compactors.
			if lvl <= 3 {
				skipGroup = true
				break
			}

			// Skip the file if it's over the max size and it contains a full block
			if gen.size() >= uint64(maxTSMFileSize) && c.FileStore.BlockCount(gen.files[0].Path, 1) == tsdb.DefaultMaxPointsPerBlock && !gen.hasTombstones() {
				startIndex++
				continue
			}
		}

		if skipGroup {
			continue
		}

		endIndex := i + step
		if endIndex > len(generations) {
			endIndex = len(generations)
		}
		if endIndex-startIndex > 0 {
			groups = append(groups, generations[startIndex:endIndex])
		}
	}

	if len(groups) == 0 {
		return nil
	}

	// With the groups, we need to evaluate whether the group as a whole can be compacted
	compactable := []tsmGenerations{}
	for _, group := range groups {
		//if we don't have enough generations to compact, skip it
		if len(group) < 2 && !group.hasTombstones() {
			continue
		}
		compactable = append(compactable, group)
	}

	// All the files to be compacted must be compacted in order.  We need to convert each
	// group to the actual set of files in that group to be compacted.
	var tsmFiles []CompactionGroup
	for _, c := range compactable {
		var cGroup CompactionGroup
		for _, group := range c {
			for _, f := range group.files {
				cGroup = append(cGroup, f.Path)
			}
		}
		sort.Strings(cGroup)
		tsmFiles = append(tsmFiles, cGroup)
	}

	return tsmFiles
}

// findGenerations groups all the TSM files by generation based
// on their filename, then returns the generations in descending order (newest first).
func (c *DefaultPlanner) findGenerations() tsmGenerations {
	c.mu.RLock()
	last := c.lastFindGenerations
	lastGen := c.lastGenerations
	c.mu.RUnlock()

	if !last.IsZero() && c.FileStore.LastModified().Equal(last) {
		return lastGen
	}

	genTime := c.FileStore.LastModified()
	tsmStats := c.FileStore.Stats()
	generations := make(map[int]*tsmGeneration, len(tsmStats))
	for _, f := range tsmStats {
		gen, _, _ := ParseTSMFileName(f.Path)

		group := generations[gen]
		if group == nil {
			group = &tsmGeneration{
				id: gen,
			}
			generations[gen] = group
		}
		group.files = append(group.files, f)
	}

	orderedGenerations := make(tsmGenerations, 0, len(generations))
	for _, g := range generations {
		orderedGenerations = append(orderedGenerations, g)
	}
	if !orderedGenerations.IsSorted() {
		sort.Sort(orderedGenerations)
	}

	c.mu.Lock()
	c.lastFindGenerations = genTime
	c.lastGenerations = orderedGenerations
	c.mu.Unlock()

	return orderedGenerations
}

// Compactor merges multiple TSM files into new files or
// writes a Cache into 1 or more TSM files.
type Compactor struct {
	Dir  string
	Size int

	FileStore interface {
		NextGeneration() int
	}

	mu                 sync.RWMutex
	snapshotsEnabled   bool
	compactionsEnabled bool

	files map[string]struct{}
}

// Open initializes the Compactor.
func (c *Compactor) Open() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshotsEnabled || c.compactionsEnabled {
		return
	}

	c.snapshotsEnabled = true
	c.compactionsEnabled = true
	c.files = make(map[string]struct{})
}

// Close disables the Compactor.
func (c *Compactor) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !(c.snapshotsEnabled || c.compactionsEnabled) {
		return
	}
	c.snapshotsEnabled = false
	c.compactionsEnabled = false
}

// DisableSnapshots disables the compactor from performing snapshots.
func (c *Compactor) DisableSnapshots() {
	c.mu.Lock()
	c.snapshotsEnabled = false
	c.mu.Unlock()
}

// EnableSnapshots allows the compactor to perform snapshots.
func (c *Compactor) EnableSnapshots() {
	c.mu.Lock()
	c.snapshotsEnabled = true
	c.mu.Unlock()
}

// DisableSnapshots disables the compactor from performing compactions.
func (c *Compactor) DisableCompactions() {
	c.mu.Lock()
	c.compactionsEnabled = false
	c.mu.Unlock()
}

// EnableCompactions allows the compactor to perform compactions.
func (c *Compactor) EnableCompactions() {
	c.mu.Lock()
	c.compactionsEnabled = true
	c.mu.Unlock()
}

// WriteSnapshot writes a Cache snapshot to one or more new TSM files.
func (c *Compactor) WriteSnapshot(cache *Cache) ([]string, error) {
	c.mu.RLock()
	enabled := c.snapshotsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errSnapshotsDisabled
	}

	iter := NewCacheKeyIterator(cache, tsdb.DefaultMaxPointsPerBlock)
	files, err := c.writeNewFiles(c.FileStore.NextGeneration(), 0, iter)

	// See if we were disabled while writing a snapshot
	c.mu.RLock()
	enabled = c.snapshotsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errSnapshotsDisabled
	}

	return files, err
}

// compact writes multiple smaller TSM files into 1 or more larger files.
func (c *Compactor) compact(fast bool, tsmFiles []string) ([]string, error) {
	size := c.Size
	if size <= 0 {
		size = tsdb.DefaultMaxPointsPerBlock
	}
	// The new compacted files need to added to the max generation in the
	// set.  We need to find that max generation as well as the max sequence
	// number to ensure we write to the next unique location.
	var maxGeneration, maxSequence int
	for _, f := range tsmFiles {
		gen, seq, err := ParseTSMFileName(f)
		if err != nil {
			return nil, err
		}

		if gen > maxGeneration {
			maxGeneration = gen
			maxSequence = seq
		}

		if gen == maxGeneration && seq > maxSequence {
			maxSequence = seq
		}
	}

	// For each TSM file, create a TSM reader
	var trs []*TSMReader
	for _, file := range tsmFiles {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}

		tr, err := NewTSMReader(f)
		if err != nil {
			return nil, err
		}
		defer tr.Close()
		trs = append(trs, tr)
	}

	if len(trs) == 0 {
		return nil, nil
	}

	tsm, err := NewTSMKeyIterator(size, fast, trs...)
	if err != nil {
		return nil, err
	}

	return c.writeNewFiles(maxGeneration, maxSequence, tsm)
}

// CompactFull writes multiple smaller TSM files into 1 or more larger files.
func (c *Compactor) CompactFull(tsmFiles []string) ([]string, error) {
	c.mu.RLock()
	enabled := c.compactionsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errCompactionsDisabled
	}

	if !c.add(tsmFiles) {
		return nil, errCompactionInProgress
	}
	defer c.remove(tsmFiles)

	files, err := c.compact(false, tsmFiles)

	// See if we were disabled while writing a snapshot
	c.mu.RLock()
	enabled = c.compactionsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errCompactionsDisabled
	}

	return files, err
}

// CompactFast writes multiple smaller TSM files into 1 or more larger files.
func (c *Compactor) CompactFast(tsmFiles []string) ([]string, error) {
	c.mu.RLock()
	enabled := c.compactionsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errCompactionsDisabled
	}

	if !c.add(tsmFiles) {
		return nil, errCompactionInProgress
	}
	defer c.remove(tsmFiles)

	files, err := c.compact(true, tsmFiles)

	// See if we were disabled while writing a snapshot
	c.mu.RLock()
	enabled = c.compactionsEnabled
	c.mu.RUnlock()

	if !enabled {
		return nil, errCompactionsDisabled
	}

	return files, err

}

// writeNewFiles writes from the iterator into new TSM files, rotating
// to a new file once it has reached the max TSM file size.
func (c *Compactor) writeNewFiles(generation, sequence int, iter KeyIterator) ([]string, error) {
	// These are the new TSM files written
	var files []string

	for {
		sequence++
		// New TSM files are written to a temp file and renamed when fully completed.
		fileName := filepath.Join(c.Dir, fmt.Sprintf("%09d-%09d.%s.tmp", generation, sequence, TSMFileExtension))

		// Write as much as possible to this file
		err := c.write(fileName, iter)

		// We've hit the max file limit and there is more to write.  Create a new file
		// and continue.
		if err == errMaxFileExceeded || err == ErrMaxBlocksExceeded {
			files = append(files, fileName)
			continue
		} else if err == ErrNoValues {
			// If the file only contained tombstoned entries, then it would be a 0 length
			// file that we can drop.
			if err := os.RemoveAll(fileName); err != nil {
				return nil, err
			}
			break
		}

		// We hit an error but didn't finish the compaction.  Remove the temp file and abort.
		if err != nil {
			return nil, err
		}

		files = append(files, fileName)
		break
	}

	return files, nil
}

func (c *Compactor) write(path string, iter KeyIterator) (err error) {
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0666)
	if err != nil {
		return errCompactionInProgress
	}

	// Create the write for the new TSM file.
	w, err := NewTSMWriter(fd)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := w.Close()
		if err == nil {
			err = closeErr
		}
	}()

	for iter.Next() {
		c.mu.RLock()
		enabled := c.snapshotsEnabled || c.compactionsEnabled
		c.mu.RUnlock()

		if !enabled {
			return errCompactionAborted
		}
		// Each call to read returns the next sorted key (or the prior one if there are
		// more values to write).  The size of values will be less than or equal to our
		// chunk size (1000)
		key, minTime, maxTime, block, err := iter.Read()
		if err != nil {
			return err
		}

		// Write the key and value
		if err := w.WriteBlock(key, minTime, maxTime, block); err == ErrMaxBlocksExceeded {
			if err := w.WriteIndex(); err != nil {
				return err
			}
			return err
		} else if err != nil {
			return err
		}

		// If we have a max file size configured and we're over it, close out the file
		// and return the error.
		if w.Size() > maxTSMFileSize {
			if err := w.WriteIndex(); err != nil {
				return err
			}

			return errMaxFileExceeded
		}
	}

	// We're all done.  Close out the file.
	if err := w.WriteIndex(); err != nil {
		return err
	}
	return nil
}

func (c *Compactor) add(files []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// See if the new files are already in use
	for _, f := range files {
		if _, ok := c.files[f]; ok {
			return false
		}
	}

	// Mark all the new files in use
	for _, f := range files {
		c.files[f] = struct{}{}
	}
	return true
}

func (c *Compactor) remove(files []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range files {
		delete(c.files, f)
	}
}

// KeyIterator allows iteration over set of keys and values in sorted order.
type KeyIterator interface {
	// Next returns true if there are any values remaining in the iterator.
	Next() bool

	// Read returns the key, time range, and raw data for the next block,
	// or any error that occurred.
	Read() (key string, minTime int64, maxTime int64, data []byte, err error)

	// Close closes the iterator.
	Close() error
}

// tsmKeyIterator implements the KeyIterator for set of TSMReaders.  Iteration produces
// keys in sorted order and the values between the keys sorted and deduped.  If any of
// the readers have associated tombstone entries, they are returned as part of iteration.
type tsmKeyIterator struct {
	// readers is the set of readers it produce a sorted key run with
	readers []*TSMReader

	// values is the temporary buffers for each key that is returned by a reader
	values map[string][]Value

	// pos is the current key postion within the corresponding readers slice.  A value of
	// pos[0] = 1, means the reader[0] is currently at key 1 in its ordered index.
	pos []int

	keys []string

	// err is any error we received while iterating values.
	err error

	// indicates whether the iterator should choose a faster merging strategy over a more
	// optimally compressed one.  If fast is true, multiple blocks will just be added as is
	// and not combined.  In some cases, a slower path will need to be utilized even when
	// fast is true to prevent overlapping blocks of time for the same key.
	// If false, the blocks will be decoded and duplicated (if needed) and
	// then chunked into the maximally sized blocks.
	fast bool

	// size is the maximum number of values to encode in a single block
	size int

	// key is the current key lowest key across all readers that has not be fully exhausted
	// of values.
	key string

	iterators []*BlockIterator
	blocks    blocks

	buf []blocks

	// mergeValues are decoded blocks that have been combined
	mergedValues Values

	// merged are encoded blocks that have been combined or used as is
	// without decode
	merged blocks
}

type block struct {
	key              string
	minTime, maxTime int64
	b                []byte
	tombstones       []TimeRange

	// readMin, readMax are the timestamps range of values have been
	// read and encoded from this block.
	readMin, readMax int64
}

func (b *block) overlapsTimeRange(min, max int64) bool {
	return b.minTime <= max && b.maxTime >= min
}

func (b *block) read() bool {
	return b.readMin <= b.minTime && b.readMax >= b.maxTime
}

func (b *block) markRead(min, max int64) {
	if min < b.readMin {
		b.readMin = min
	}

	if max > b.readMax {
		b.readMax = max
	}
}

type blocks []*block

func (a blocks) Len() int { return len(a) }

func (a blocks) Less(i, j int) bool {
	if a[i].key == a[j].key {
		return a[i].minTime < a[j].minTime
	}
	return a[i].key < a[j].key
}

func (a blocks) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// NewTSMKeyIterator returns a new TSM key iterator from readers.
// size indicates the maximum number of values to encode in a single block.
func NewTSMKeyIterator(size int, fast bool, readers ...*TSMReader) (KeyIterator, error) {
	var iter []*BlockIterator
	for _, r := range readers {
		iter = append(iter, r.BlockIterator())
	}

	return &tsmKeyIterator{
		readers:   readers,
		values:    map[string][]Value{},
		pos:       make([]int, len(readers)),
		size:      size,
		iterators: iter,
		fast:      fast,
		buf:       make([]blocks, len(iter)),
	}, nil
}

// Next returns true if there are any values remaining in the iterator.
func (k *tsmKeyIterator) Next() bool {
	// Any merged blocks pending?
	if len(k.merged) > 0 {
		k.merged = k.merged[1:]
		if len(k.merged) > 0 {
			return true
		}
	}

	// Any merged values pending?
	if len(k.mergedValues) > 0 {
		k.merge()
		if len(k.merged) > 0 || len(k.mergedValues) > 0 {
			return true
		}
	}

	// If we still have blocks from the last read, merge them
	if len(k.blocks) > 0 {
		k.merge()
		if len(k.merged) > 0 || len(k.mergedValues) > 0 {
			return true
		}
	}

	// Read the next block from each TSM iterator
	for i, v := range k.buf {
		if v == nil {
			iter := k.iterators[i]
			if iter.Next() {
				key, minTime, maxTime, _, b, err := iter.Read()
				if err != nil {
					k.err = err
				}

				// This block may have ranges of time removed from it that would
				// reduce the block min and max time.
				tombstones := iter.r.TombstoneRange(key)
				k.buf[i] = append(k.buf[i], &block{
					minTime:    minTime,
					maxTime:    maxTime,
					key:        key,
					b:          b,
					tombstones: tombstones,
					readMin:    math.MaxInt64,
					readMax:    math.MinInt64,
				})

				blockKey := key
				for iter.PeekNext() == blockKey {
					iter.Next()
					key, minTime, maxTime, _, b, err := iter.Read()
					if err != nil {
						k.err = err
					}

					tombstones := iter.r.TombstoneRange(key)

					k.buf[i] = append(k.buf[i], &block{
						minTime:    minTime,
						maxTime:    maxTime,
						key:        key,
						b:          b,
						tombstones: tombstones,
						readMin:    math.MaxInt64,
						readMax:    math.MinInt64,
					})
				}
			}
		}
	}

	// Each reader could have a different key that it's currently at, need to find
	// the next smallest one to keep the sort ordering.
	var minKey string
	for _, b := range k.buf {
		// block could be nil if the iterator has been exhausted for that file
		if len(b) == 0 {
			continue
		}
		if minKey == "" || b[0].key < minKey {
			minKey = b[0].key
		}
	}
	k.key = minKey

	// Now we need to find all blocks that match the min key so we can combine and dedupe
	// the blocks if necessary
	for i, b := range k.buf {
		if len(b) == 0 {
			continue
		}
		if b[0].key == k.key {
			k.blocks = append(k.blocks, b...)
			k.buf[i] = nil
		}
	}

	if len(k.blocks) == 0 {
		return false
	}

	k.merge()

	return len(k.merged) > 0
}

// merge combines the next set of blocks into merged blocks.
func (k *tsmKeyIterator) merge() {
	// No blocks left, or pending merged values, we're done
	if len(k.blocks) == 0 && len(k.merged) == 0 && len(k.mergedValues) == 0 {
		return
	}

	dedup := false
	if len(k.blocks) > 0 {
		// If we have more than one block or any partially tombstoned blocks, we many need to dedup
		dedup = len(k.blocks[0].tombstones) > 0

		if len(k.blocks) > 1 {
			// Quickly scan each block to see if any overlap with the prior block, if they overlap then
			// we need to dedup as there may be duplicate points now
			for i := 1; !dedup && i < len(k.blocks); i++ {
				if k.blocks[i].read() {
					dedup = true
					break
				}
				if k.blocks[i].minTime <= k.blocks[i-1].maxTime || len(k.blocks[i].tombstones) > 0 {
					dedup = true
					break
				}
			}
		}
	}

	k.merged = k.combine(dedup)
}

// combine returns a new set of blocks using the current blocks in the buffers.  If dedup
// is true, all the blocks will be decoded, dedup and sorted in in order.  If dedup is false,
// only blocks that are smaller than the chunk size will be decoded and combined.
func (k *tsmKeyIterator) combine(dedup bool) blocks {
	if dedup {
		for len(k.mergedValues) < k.size && len(k.blocks) > 0 {
			for len(k.blocks) > 0 && k.blocks[0].read() {
				k.blocks = k.blocks[1:]
			}

			if len(k.blocks) == 0 {
				break
			}
			first := k.blocks[0]

			// We have some overlapping blocks so decode all, append in order and then dedup
			for i := 0; i < len(k.blocks); i++ {
				if !k.blocks[i].overlapsTimeRange(first.minTime, first.maxTime) || k.blocks[i].read() {
					continue
				}

				v, err := DecodeBlock(k.blocks[i].b, nil)
				if err != nil {
					k.err = err
					return nil
				}

				// Remove values we already read
				v = Values(v).Exclude(k.blocks[i].readMin, k.blocks[i].readMax)

				// Filter out only the values for overlapping block
				v = Values(v).Include(first.minTime, first.maxTime)
				if len(v) > 0 {
					// Record that we read a subset of the block
					k.blocks[i].markRead(v[0].UnixNano(), v[len(v)-1].UnixNano())
				}

				// Apply each tombstone to the block
				for _, ts := range k.blocks[i].tombstones {
					v = Values(v).Exclude(ts.Min, ts.Max)
				}

				k.mergedValues = k.mergedValues.Merge(v)
			}
			k.blocks = k.blocks[1:]
		}

		// Since we combined multiple blocks, we could have more values than we should put into
		// a single block.  We need to chunk them up into groups and re-encode them.
		return k.chunk(nil)
	} else {
		var chunked blocks
		var i int

		for i < len(k.blocks) {
			// skip this block if it's values were already read
			if k.blocks[i].read() {
				i++
				continue
			}
			// If we this block is already full, just add it as is
			if BlockCount(k.blocks[i].b) >= k.size {
				chunked = append(chunked, k.blocks[i])
			} else {
				break
			}
			i++
		}

		if k.fast {
			for i < len(k.blocks) {
				// skip this block if it's values were already read
				if k.blocks[i].read() {
					i++
					continue
				}

				chunked = append(chunked, k.blocks[i])
				i++
			}
		}

		// If we only have 1 blocks left, just append it as is and avoid decoding/recoding
		if i == len(k.blocks)-1 {
			if !k.blocks[i].read() {
				chunked = append(chunked, k.blocks[i])
			}
			i++
		}

		// The remaining blocks can be combined and we know that they do not overlap and
		// so we can just append each, sort and re-encode.
		for i < len(k.blocks) && len(k.mergedValues) < k.size {
			if k.blocks[i].read() {
				i++
				continue
			}

			v, err := DecodeBlock(k.blocks[i].b, nil)
			if err != nil {
				k.err = err
				return nil
			}

			// Apply each tombstone to the block
			for _, ts := range k.blocks[i].tombstones {
				v = Values(v).Exclude(ts.Min, ts.Max)
			}

			k.blocks[i].markRead(k.blocks[i].minTime, k.blocks[i].maxTime)

			k.mergedValues = k.mergedValues.Merge(v)
			i++
		}

		k.blocks = k.blocks[i:]

		return k.chunk(chunked)
	}
}

func (k *tsmKeyIterator) chunk(dst blocks) blocks {
	if len(k.mergedValues) > k.size {
		values := k.mergedValues[:k.size]
		cb, err := Values(values).Encode(nil)
		if err != nil {
			k.err = err
			return nil
		}

		dst = append(dst, &block{
			minTime: values[0].UnixNano(),
			maxTime: values[len(values)-1].UnixNano(),
			key:     k.key,
			b:       cb,
		})
		k.mergedValues = k.mergedValues[k.size:]
		return dst
	}

	// Re-encode the remaining values into the last block
	if len(k.mergedValues) > 0 {
		cb, err := Values(k.mergedValues).Encode(nil)
		if err != nil {
			k.err = err
			return nil
		}

		dst = append(dst, &block{
			minTime: k.mergedValues[0].UnixNano(),
			maxTime: k.mergedValues[len(k.mergedValues)-1].UnixNano(),
			key:     k.key,
			b:       cb,
		})
		k.mergedValues = k.mergedValues[:0]
	}
	return dst
}

func (k *tsmKeyIterator) Read() (string, int64, int64, []byte, error) {
	if len(k.merged) == 0 {
		return "", 0, 0, nil, k.err
	}

	block := k.merged[0]
	return block.key, block.minTime, block.maxTime, block.b, k.err
}

func (k *tsmKeyIterator) Close() error {
	k.values = nil
	k.pos = nil
	k.iterators = nil
	for _, r := range k.readers {
		if err := r.Close(); err != nil {
			return err
		}
	}
	return nil
}

type cacheKeyIterator struct {
	cache *Cache
	size  int
	order []string

	i      int
	blocks [][]cacheBlock
	ready  []chan struct{}
}

type cacheBlock struct {
	k                string
	minTime, maxTime int64
	b                []byte
	err              error
}

// NewCacheKeyIterator returns a new KeyIterator from a Cache.
func NewCacheKeyIterator(cache *Cache, size int) KeyIterator {
	keys := cache.Keys()

	chans := make([]chan struct{}, len(keys))
	for i := 0; i < len(keys); i++ {
		chans[i] = make(chan struct{}, 1)
	}

	cki := &cacheKeyIterator{
		i:      -1,
		size:   size,
		cache:  cache,
		order:  keys,
		ready:  chans,
		blocks: make([][]cacheBlock, len(keys)),
	}
	go cki.encode()
	return cki
}

func (c *cacheKeyIterator) encode() {
	concurrency := runtime.GOMAXPROCS(0)
	n := len(c.ready)

	// Divide the keyset across each CPU
	chunkSize := 128
	idx := uint64(0)
	for i := 0; i < concurrency; i++ {
		// Run one goroutine per CPU and encode a section of the key space concurrently
		go func() {
			for {
				start := int(atomic.AddUint64(&idx, uint64(chunkSize))) - chunkSize
				if start >= n {
					break
				}
				end := start + chunkSize
				if end > n {
					end = n
				}
				c.encodeRange(start, end)
			}
		}()
	}
}

func (c *cacheKeyIterator) encodeRange(start, stop int) {
	for i := start; i < stop; i++ {
		key := c.order[i]
		values := c.cache.values(key)

		for len(values) > 0 {
			minTime, maxTime := values[0].UnixNano(), values[len(values)-1].UnixNano()
			var b []byte
			var err error
			if len(values) > c.size {
				maxTime = values[c.size-1].UnixNano()
				b, err = Values(values[:c.size]).Encode(nil)
				values = values[c.size:]
			} else {
				b, err = Values(values).Encode(nil)
				values = values[:0]
			}
			c.blocks[i] = append(c.blocks[i], cacheBlock{
				k:       key,
				minTime: minTime,
				maxTime: maxTime,
				b:       b,
				err:     err,
			})
		}
		// Notify this key is fully encoded
		c.ready[i] <- struct{}{}
	}
}

func (c *cacheKeyIterator) Next() bool {
	if c.i >= 0 && c.i < len(c.ready) && len(c.blocks[c.i]) > 0 {
		c.blocks[c.i] = c.blocks[c.i][1:]
		if len(c.blocks[c.i]) > 0 {
			return true
		}
	}
	c.i++

	if c.i >= len(c.ready) {
		return false
	}

	<-c.ready[c.i]
	return true
}

func (c *cacheKeyIterator) Read() (string, int64, int64, []byte, error) {
	blk := c.blocks[c.i][0]
	return blk.k, blk.minTime, blk.maxTime, blk.b, blk.err
}

func (c *cacheKeyIterator) Close() error {
	return nil
}

type tsmGenerations []*tsmGeneration

func (a tsmGenerations) Len() int           { return len(a) }
func (a tsmGenerations) Less(i, j int) bool { return a[i].id < a[j].id }
func (a tsmGenerations) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a tsmGenerations) hasTombstones() bool {
	for _, g := range a {
		if g.hasTombstones() {
			return true
		}
	}
	return false
}

func (a tsmGenerations) chunk(size int) []tsmGenerations {
	var chunks []tsmGenerations
	for len(a) > 0 {
		if len(a) >= size {
			chunks = append(chunks, a[:size])
			a = a[size:]
		} else {
			chunks = append(chunks, a)
			a = a[len(a):]
		}
	}
	return chunks
}

func (a tsmGenerations) IsSorted() bool {
	if len(a) == 1 {
		return true
	}

	for i := 1; i < len(a); i++ {
		if a.Less(i, i-1) {
			return false
		}
	}
	return true
}
