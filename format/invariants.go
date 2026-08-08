package format

// The canonicality table.
//
// Every rule that decides whether two files holding the same content are the
// same bytes lives here, in one place, with the specification sentences it
// comes from and the test that enforces it. The specification and the code are
// otherwise two independent descriptions of the format, and every review round
// has found somewhere they drifted: a rule stated in one and not the other, a
// paragraph left behind when what it described was replaced.
//
// Rule ids are stable handles for the normative sentences of format.md,
// derived from their text (see specrules_test.go). Changing a sentence changes
// its id, so the table stops matching and the rule has to be looked at again.
//
// Category says what kind of rule it is, which is also the checklist for
// reviewing any new wire structure: what must be present, what bounds it, how
// it is ordered, when it is left out, and what shape it is normalised into.
type Category string

const (
	// Presence: a field must exist, or must not.
	Presence Category = "presence"
	// Bound: a value has a floor or a ceiling.
	Bound Category = "bound"
	// Ordering: elements have one permitted order.
	Ordering Category = "ordering"
	// Omission: content may or must be left out, and absence has a meaning.
	Omission Category = "omission"
	// Normalisation: one value has one spelling.
	Normalisation Category = "normalisation"
	// Integrity: hashes, chains and recovery.
	Integrity Category = "integrity"
)

// Enforcement says who is obliged to uphold a rule. Leaving this implicit is
// how a strict reader and a lenient one end up disagreeing about which files
// are valid, which is the failure §7 of the specification names for metadata
// and which applies just as well everywhere else.
type Enforcement string

const (
	// Decoded means readers reject a file that breaks the rule. Most rules are
	// of this kind and it is the default a reviewer should expect.
	Decoded Enforcement = "decoded"
	// WriterOnly means a reader cannot check the rule, because the evidence is
	// not in the file. Palette ordering is the archetype: reference counts are
	// never stored, so nothing in a file proves its palette was sorted. Such a
	// rule is verified by re-encoding and comparing (format.ContentHash), not
	// by reading, and saying so is the point: a rule nobody can check on read
	// must not look like one that is checked.
	WriterOnly Enforcement = "writer-only"
)

// Invariant is one canonicality rule.
type Invariant struct {
	// Name is a short handle used in failures and commit messages.
	Name string
	// Category is what kind of rule this is.
	Category Category
	// Enforce says whether readers reject violations or the rule binds only
	// writers. Every entry states it; an empty value fails the harness.
	Enforce Enforcement
	// Rules are the ids of the format.md sentences this covers.
	Rules []string
	// Tests name every test function that enforces it. A rule with a writer
	// half and a reader half, or one that applies to both worlds and
	// structures, needs one per half: naming a single test that covers one
	// side is a claim the other side is protected when it is not.
	Tests []string
	// Note explains anything the specification sentence leaves implicit,
	// usually why the rule exists rather than what it says.
	Note string
}

// invariants is the canonicality table. Each entry claims the normative
// sentences it covers and names the test that enforces it; specrules_test.go
// checks both directions, so a rule with no entry and an entry naming a dead
// test are both failures.
var invariants = []Invariant{
	// -- Primitives (§1) ------------------------------------------------
	{
		Name: "varints are minimal", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"b5904bd9", "b970a5c4"},
		Tests: []string{"TestRejectsNonMinimalVarints"},
		Note:  "An overlong encoding is a second spelling of one number.",
	},
	{
		Name: "strings are bounded UTF-8", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"787dd1f6"},
		Tests: []string{"TestRejectsNonUTF8Strings", "TestReaderRejectsBadStrings"},
		Note:  "Palettes order strings bytewise, so arbitrary bytes would order differently under an implementation that decodes before comparing.",
	},
	{
		Name: "blobs are bounded", Category: Bound, Enforce: Decoded,
		Rules: []string{"f930c4ef"},
		Tests: []string{"TestRejectsOversizedBlob"},
		Note:  "The blob primitive, not the lengths inside an NBT payload: the validator's own tests never reach the container's length prefix.",
	},
	{
		Name: "bitset padding is zero", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"cf8e4eb4"},
		Tests: []string{"TestRejectsBitsetPadding", "TestRejectsLightBitsetPadding"},
		Note:  "Bits above the count carry no meaning, so a set one is a second encoding. Checked through every parser that reads a bitset, not through the helper they call: a parser that stopped calling it would leave the helper's own test green.",
	},
	{
		Name: "NBT compounds are canonical", Category: Ordering, Enforce: Decoded,
		Rules: []string{"2811e0b4", "e01f257a"},
		Tests: []string{"TestRejectsNonCanonicalNBT"},
		Note:  "Unique keys in ascending order is what lets independent writers agree.",
	},
	{
		Name: "array tags are distinct from lists", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"dce0103c"},
		Tests: []string{"TestOpaqueNBTArraysSurvive"},
		Note:  "Bedrock stores UUIDs as int arrays; folding them into lists is lossy.",
	},
	{
		Name: "metadata field tags are exact", Category: Presence, Enforce: Decoded,
		Rules: []string{"f1047fc4", "7f907b36", "bf4e7d6b"},
		Tests: []string{"TestRejectsMetaSchemaViolations", "TestReaderEnforcesMetadataSchemas"},
	},

	// -- Header and container (§2) ---------------------------------------
	{
		Name: "unknown versions and flags are rejected", Category: Presence, Enforce: Decoded,
		Rules: []string{"a4590032", "648be581"},
		Tests: []string{"TestRejectsReservedFlags", "TestIndexedRejectsReservedFlags", "TestIndexedRejectsUnsupportedVersion"},
		Note:  "Ignoring a reserved bit spends it: an old reader could not tell a future file needs a feature it lacks. The version has a reader per mode: the indexed one checks it before the directory is consulted, so the recovery that repairs a damaged kind or mode never reaches it and the solid reader's fixture says nothing about it.",
	},
	{
		Name: "blockVersion names a version", Category: Presence, Enforce: Decoded,
		Rules: []string{"718f271d"},
		Tests: []string{"TestRejectsZeroBlockVersion"},
		Note:  "Zero is taken: it is what a palette entry's version override uses to mean \"the palette's own version\", so a field carrying it says nothing rather than something. Held apart from the version and flag rules because it has its own two readers -- the physical header and the directory prologue that is authoritative over it -- and a fixture for either says nothing about the other.",
	},
	{
		Name: "the dictionary bit stays reserved", Category: Presence, Enforce: Decoded,
		Rules: []string{"2b1d7218"},
		Tests: []string{"TestRejectsReservedFlags"},
	},
	{
		Name: "the default biome reference needs its flag", Category: Presence, Enforce: Decoded,
		Rules: []string{"6dd8dd9c"},
		Tests: []string{"TestRejectsReservedFlags"},
	},
	{
		Name: "kind and mode pairs are enumerated", Category: Presence, Enforce: Decoded,
		Rules: []string{"62b04284"},
		Tests: []string{"TestRejectsIndexedStructureKind", "TestIndexedRejectsStructureKindInDirectory", "TestRejectsUndefinedKind"},
		Note:  "A structure is always solid, so an indexed structure names a layout that does not exist. The sentence also covers every value of either field the table does not list, which is a separate check from the pairing and needs its own fixture.",
	},
	{
		Name: "an NBT string fits what a Bedrock reader can address", Category: Bound, Enforce: Decoded,
		Rules: []string{"51ab5d05"},
		Tests: []string{"TestRejectsOversizedNBTString", "TestNBTWriterHoldsTheContainerBudget"},
		Note: "The length field is a u16 and expresses 65,535, and the Bedrock NBT readers in practical use take it as a signed int16, so 32,768 and up arrive negative. This is the one place the specification's stated ceiling and its reachable one differed, and it was found from both sides at once: a writer emitting a 69,780-byte block-entity blob produced a file ReadWorld refuses, silently, with StoreColumn and Close both returning nil. " +
			"Enforced here rather than left to the dependency, which refuses the same blobs with \"unexpected buffer end\" several layers from the cause. Same accept/reject boundary, an error that names the rule. Values and compound keys are separate checks because they are read by separate functions; a fixture for one says nothing about the other.",
	},
	{
		Name: "the retired dimension bits are reserved", Category: Presence, Enforce: Decoded,
		Rules: []string{"8498fc02"},
		Tests: []string{"TestRejectsRetiredDimensionBits"},
		Note: "Bits 5-7 held a dimension field until it was removed before the freeze. The rule they are held to now is the general one every reserved bit has: knownFlags does not list them, and an unlisted bit fails ErrUnknownFlags in both readers. " +
			"It keeps an entry rather than vanishing because the removal has a writer half as well as a reader half, and the writer half is the one that fails silently: a path that kept setting a bit would emit a file it cannot read back. The named test covers both halves for this package's writers. " +
			"The provider's three writers are walked by TestNoWriterSetsTheReservedFlagBits in the root package, which this table cannot name because the harness only resolves tests declared in format.",
	},
	{
		Name: "solid footers carry no indexed words", Category: Presence, Enforce: Decoded,
		Rules: []string{"af37ce22"},
		Tests: []string{"TestSolidFooterMustBeZero"},
	},

	// -- Palettes and blobs (§3) -----------------------------------------
	{
		Name: "the palette order is defined on encoded bytes", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"ad7fe57e", "3bc3297f"},
		Tests: []string{"TestPaletteOrderFollowsEncodedBytes"},
		Note: "The tie-break is the entry's own bytes, so no implementation has to agree on a string form first. This is the rule that decides every palette index and therefore every section blob. " +
			"Missing evidence: the reference count each entry was sorted on. It is not a field anywhere in the format, and any permutation of a palette is consistent with some assignment of counts, so nothing in a file distinguishes a sorted palette from a shuffled one. §3.1 goes further and forbids readers from trying, which matters because a reader could plausibly reconstruct the block counts by walking every stored local palette; doing so would make files this version writes depend on a reconstruction the specification never defined, and it is the one place here where 'a reader could' is not the same as 'a reader may'. Verified by re-encoding.",
	},
	{
		Name: "state properties are ordered and unique", Category: Ordering, Enforce: Decoded,
		Rules: []string{"5ed7d87f"},
		Tests: []string{"TestRejectsUnorderedStateProperties", "TestWriterSortsStateProperties"},
		Note:  "A repeated key is worse than an out-of-order one: the later value silently wins, so two files decode to one state.",
	},
	{
		Name: "palette entries are unique", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"afea490c", "ddf5c7be"},
		Tests: []string{"TestPaletteMergesIdenticalEntries", "TestRejectsDuplicatePaletteEntries"},
		Note:  "Two entries that encode identically at one version have nothing to order them by.",
	},
	{
		Name: "version overrides mean a different version", Category: Omission, Enforce: Decoded,
		Rules: []string{"2062adb1", "871174ff"},
		Tests: []string{"TestVersionZeroRoundTrips", "TestRejectsRedundantVersionOverride"},
		Note:  "An override equal to the palette's own version is redundant and grows a round trip that changed nothing.",
	},
	{
		Name: "version override indices strictly ascend", Category: Ordering, Enforce: Decoded,
		Rules: []string{"2a4c2ef2"},
		Tests: []string{"TestHostileOverrideDeltaWraps", "TestConformanceVectorsNegative"},
		Note: "The ascent was stated only as an annotation inside §3.1's layout fence, and the extractor strips fences, so no sentence existed for an entry to claim and the rule went unpinned while a reader enforced half of it. That half -- a later delta MUST be non-zero -- holds exactly as long as the running sum cannot wrap, and it can: the sum is a uint64 and a uvarint can carry the modular representative of a negative step, so a delta of 2^64-2 after index 5 lands on index 3. " +
			"Three tests because there are three readers. The production decoder, the independent walker of the vector suite (which had the same wrap and now refuses it), and the zero-delta case, which is a different input and stays a separate check: a zero delta and a wrap are distinguishable, so both are enforcement rather than one check wearing two names.",
	},
	{
		Name: "reference counts predate deduplication", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{},
		Tests: []string{"TestPaletteCountsOccurrencesNotBlobs"},
		Note: "Counting distinct blobs instead reorders the palette exactly when deduplication succeeds. " +
			"Missing evidence: the counts themselves, as for the palette order above. Both spellings of the count produce a palette that is a legal permutation of the other, and a file records neither the counts nor which of the two rules produced its order. Verified by re-encoding.",
	},
	{
		Name: "section blobs are canonical", Category: Ordering, Enforce: Decoded,
		Rules: []string{"5ab4d3d6", "5240768e", "e72263a7", "9c7d6645", "34f2544c"},
		Tests: []string{"TestRejectsNonCanonicalBlob", "TestRejectsOutOfRangePaletteIndex", "TestRejectsUnusedLocalPaletteEntry", "TestRejectsNonMinimalUniformWidth"},
		Note:  "The blob decoder checks the palette, the width and that every entry is named by some index; whether an index is in range is checked only where the section is applied, and each width has its own loop, so the halves need separate fixtures. The used-entry half is load-bearing for other rules: uniformity is read off the local palette's length, so an unused entry is how a present all-air section, a trailing air layer or a uniform-default biome section gets past the rules that require it to be absent. It is also why every fixture here has to name each entry it declares: a blob that leaves one unnamed is refused by the used-entry rule before the rule under test runs, which is how the ascent and width checks were once claimed by fixtures that never reached them.",
	},
	{
		Name: "identical blobs share one table entry", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"1e589410"},
		Tests: []string{"TestDedup", "TestRejectsBlobTableWaste", "TestRejectsUnreferencedBlob", "TestDecodersAgreeOnValidity"},
		Note:  "The rule applies wherever a blob table does, and a structure has one of its own. Its reader resolved cell references with a bare index for a long time, tracking neither what was used nor in what order, so the world fixtures said nothing about it.",
	},

	// -- Chunk records (§4) ----------------------------------------------
	{
		Name: "biome counts predate elision", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"27ea7bc1"},
		Tests: []string{"TestBiomeCountsPrecedeElision"},
		Note: "Elision depends on the palette order, which depends on the counts: counting before elision is what stops that being circular. " +
			"Missing evidence: the elided sections. They are what the count was taken over and they are, by the rule's own definition, absent from the file, so a reader has strictly less material than the writer had and cannot recompute the number it sorted on. This is the strongest writer-only case in the table: the evidence is not merely unrecorded, it is the thing the format leaves out. Verified by re-encoding.",
	},
	{
		Name: "the section span is addressable", Category: Bound, Enforce: Decoded,
		Rules: []string{},
		Tests: []string{"TestRejectsUnaddressableSectionSpan"},
		Note:  "sectionN bounds how tall a chunk is, not where it sits.",
	},
	{
		Name: "biome names are fully qualified", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"249dada6"},
		Tests: []string{"TestBiomeNamesAreNamespaced"},
		Note:  "A bare name is not a stable identifier outside the registry that coined it.",
	},
	{
		Name: "chunk positions are unique and ascending", Category: Ordering, Enforce: Decoded,
		Rules: []string{"4e0f8a1e"},
		Tests: []string{"TestRejectsDuplicateChunks", "TestReaderRejectsUnorderedRecords"},
	},
	{
		Name: "the body ends where the last record ends", Category: Presence, Enforce: Decoded,
		Rules: []string{"bd83e312"},
		Tests: []string{"TestRejectsTrailingBytesAfterBody", "TestConformanceVectorsNegative"},
		Note:  "Both readers of a solid body enforced this before the specification said it; §4 stated it without a MUST, so nothing pinned it and no entry claimed it. It is the §5.1 frame rule applied to the one body a solid file has, and it matters for the same reason: a body's length is not a field, so padding decodes to the same world under a different checkpoint hash. The world reader and the structure reader share no code here, so each has its own fixture, and the negative vector is the half a second implementation can check itself against.",
	},
	{
		Name: "the section span is never trimmed", Category: Presence, Enforce: WriterOnly,
		Note:  "A reader cannot know the span a dimension intended, only the one the record declares, so a trimmed record is indistinguishable from an honest one. Verified by re-encoding.",
		Rules: []string{"fcf0a219"},
		Tests: []string{"TestEmptyChunkKeepsFullSpan"},
	},
	{
		Name: "layer counts are addressable", Category: Bound, Enforce: Decoded,
		Rules: []string{"af4b369f"},
		Tests: []string{"TestRejectsLayerCountInRecords", "TestRejectsLayerCountInCells"},
		Note:  "A 256th layer wedges any implementation that indexes layers with a byte. Placed where a record actually carries the count, so the rule survives a parser that stops consulting the shared bound. Both fixtures supply a reference behind every layer they declare, or the read runs out of bytes and the ceiling is never reached.",
	},
	{
		Name: "trailing air layers go, internal ones stay", Category: Omission, Enforce: Decoded,
		Rules: []string{"a9ee3ac1", "673fb00d", "12bc11b2", "90e4364e"},
		Tests: []string{"TestInternalAirLayerSurvives", "TestTrailingAirLayersDropped", "TestStructureInternalAirLayerSurvives", "TestRejectsNonCanonicalSections", "TestDecodersAgreeOnValidity"},
		Note:  "Layer numbers are semantic: dropping an internal one turns waterlogging into a liquid block.",
	},
	{
		Name: "light entries describe something", Category: Presence, Enforce: Decoded,
		Rules: []string{"929864f4"},
		Tests: []string{"TestRejectsLightEntryFlags"},
	},
	{
		Name: "light entries set no reserved bits", Category: Presence, Enforce: Decoded,
		Rules: []string{"2ce6e9db"},
		Tests: []string{"TestRejectsLightEntryFlags"},
		Note: "Separate from the entry above because the inputs are distinguishable: flags == 0 is an entry carrying nothing, flags == 0x04 is an entry carrying block light and a bit this version does not define. One check would answer both and neither could fail alone. " +
			"Like the ascent rule, this lived only as an annotation inside a layout fence, so nothing pinned it while the reader enforced it. TestNoNormativeTextInFences now refuses that shape.",
	},
	{
		Name: "UniqueID is stored verbatim", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"8e623ce0"},
		Tests: []string{"TestEntityIDZeroRoundTrips"},
		Note:  "Zero is legal; rewriting it breaks encode, decode, encode.",
	},
	{
		Name: "the default biome flag is not optional", Category: Omission, Enforce: WriterOnly,
		Rules: []string{"13221d37"},
		Tests: []string{"TestUnknownDefaultBiomePreserved", "TestDefaultBiomeFlagIsSet"},
		Note:  "Declining the flag is a second encoding of the same world, but a file that declined it decodes to that world all the same and nothing in it says which sections were uniform before the writer chose. Verified by re-encoding.",
	},
	{
		Name: "uniform-default biome sections are omitted", Category: Omission, Enforce: Decoded,
		Rules: []string{"7984f33b"},
		Tests: []string{"TestRejectsStoredDefaultBiomeSection", "TestUnknownDefaultBiomePreserved"},
		Note:  "Held apart from the flag decision, which is genuinely writer-only, because this half is not. Once a file has set the flag it has named a reference, and a present blob uniform on that reference is a section the file promised to omit -- all of it on the wire, none of it needing to know what the writer was thinking. Filing the two together let the flag's argument stand in for this one and nothing checked it at all.",
	},
	{
		Name: "the biome fallback is version stable", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"d3997566"},
		Tests: []string{"TestAbsentBiomeFallbackIsVersionStable"},
		Note:  "A numeric id is a property of the running game version, so naming one would let a single file decode to different biomes on two runtimes. The expectation is resolved from the name through the registry: taking it from the function under test moved with it, and biomeName is unusable as an oracle because it answers plains for any id no biome has. What no test can separate is the last-resort literal inside the fallback, which is the id plains already holds on every runtime that has it.",
	},
	{
		Name: "elided biome sections keep their names", Category: Omission, Enforce: Decoded,
		Rules: []string{"ff694fe9"},
		Tests: []string{"TestUnknownDefaultBiomePreserved"},
	},
	{
		Name: "collections are totally ordered", Category: Ordering, Enforce: Decoded,
		Rules: []string{"920faee4"},
		Tests: []string{"TestCollectionTiesUseWrittenBytes", "TestTiedTicksAndStructureCollections", "TestReaderEnforcesCollectionOrder", "TestDecodersAgreeOnValidity"},
		Note: "Ties break on the bytes that get written, not on the caller's value. " +
			"Only two of the five orders §4.8 lists have a tie-break with legal input: entities, whose IDs are not required to be unique, and scheduled updates, which tie on position and firing tick and break on the palette reference. The NBT tie-breaks §4.8 names for world and structure block entities cannot be reached at all, because both writers refuse two entries at one position and both readers refuse the file, so a tie is exactly the input neither will produce. They are kept because the specification states them, not because anything can exercise them.",
	},

	// -- Indexed mode (§5) -----------------------------------------------
	{
		Name: "absent frame references are all zero", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"58866f01"},
		Tests: []string{"TestRejectsMalformedFrameReference"},
	},
	{
		Name: "the directory prologue is authoritative", Category: Integrity, Enforce: Decoded,
		Rules: []string{"cf48aa12"},
		Tests: []string{"TestIndexedRecoversDamagedKindAndMode", "TestIndexedRejectsStructureKindInDirectory", "TestIndexedRejectsReservedFlags"},
		Note:  "Two obligations in one sentence. The SHOULD half is that a prologue disagreeing with an intact physical header is reported rather than fatal; the MUST half is that a prologue disagreeing with §5.7 is refused outright, whatever the physical header says. Naming only the recovery test claimed the second half was protected by a test that passes precisely when nothing is rejected.",
	},
	{
		Name: "recovered writers hash the rebuilt header", Category: Integrity, Enforce: Decoded,
		Rules: []string{"5befe4df"},
		Tests: []string{"TestRecoveredHeaderCheckpointVerifies"},
	},
	{
		Name: "the directory and dictionary skip the dictionary", Category: Presence, Enforce: Decoded,
		Rules: []string{"7c4f5f2c"},
		Tests: []string{"TestIndexedDictionary"},
	},
	{
		Name: "indexed files claim only what they can hold", Category: Presence, Enforce: Decoded,
		Rules: []string{"d2702006"},
		Tests: []string{"TestIndexedRejectsSolidOnlyFlags"},
		Note:  "Indexed mode has no stats field and elides no biome sections.",
	},
	{
		Name: "a dictionary needs compression", Category: Presence, Enforce: Decoded,
		Rules: []string{"fd0cd567"},
		Tests: []string{"TestIndexedRejectsDictionaryWhenUncompressed"},
		Note:  "Independently reachable, which this entry used to deny. The fixture is an uncompressed world that installs a real trained dictionary: the frame is then stored raw, so the reference resolves and hashes, and the rule is the only thing left to refuse it. The earlier fixture pointed the reference at one arbitrary byte, which the frame checksum rejected before this rule ran, so it passed with the rule deleted. TestRejectsMalformedDictionaryReference keeps that case, which is a different check at a different point.",
	},
	{
		Name: "frames end where their content ends", Category: Presence, Enforce: Decoded,
		Rules: []string{"b108c77f"},
		Tests: []string{"TestRejectsTrailingBytesInFrames"},
		Note:  "A frame's length and hash are recorded in the directory, so padding it changes the file without changing anything it holds. The record and directory frames rejected padding; the meta frame and both kinds of palette segment did not, and each is read by a function that shares this check with none of the others, so the fixture drives all five. The dictionary frame is outside the rule: it holds opaque Zstandard bytes with no structure to end.",
	},
	{
		Name: "directory offsets accumulate in range", Category: Bound, Enforce: Decoded,
		Rules: []string{},
		Tests: []string{"TestRejectsWrappingDirectoryOffset"},
		Note:  "The positions in a directory entry are deltas that get range-checked at every step; the frame offset beside them is a delta that was checked only at the end, and the check adds the entry's length before comparing, so an offset near the top of int64 wrapped past it. Not a validity rule -- adopting a checkpoint reads every record it names, so such a file never opened either way -- but a bound the reader sizes a buffer from before it reads. Its test asserts which check refuses the file, because a verdict-only assertion passes with the bound deleted.",
	},
	{
		Name: "palette limits are cumulative", Category: Bound, Enforce: Decoded,
		Rules: []string{"ba81d481", "89ca9097"},
		Tests: []string{"TestRejectsDuplicateSegmentReference", "TestRejectsUnorderedSegmentReferences", "TestRejectsCumulativePaletteOverflow"},
		Note:  "Duplicate references, the segment order and the frame-total bound have fixtures. The order is checkable because frames are only appended, so the order they were written in is ascending offset; its strict ascent is also what refuses a duplicate, since two references to one frame share an offset, and the ordering fixture uses distinct descending offsets because a duplicate check cannot see those. The million-entry ceiling itself is reachable only by fuzzing, since no fixture builds a palette that large; that is stated rather than papered over with a test that would pass either way.",
	},

	// -- Structures (§6) --------------------------------------------------
	{
		Name: "a structure origin fits its own type", Category: Bound, Enforce: Decoded,
		Rules: []string{"6a0a223a"},
		Tests: []string{"TestRejectsStructureOriginOutsideInt32", "TestStructureOriginExtremes"},
		Note: "The origin is three svarints and the field it lands in is an int32, so the wire can express values the structure cannot hold. Narrowing silently would fold two wire values onto one origin, which is the canonicality failure, not merely a wrong coordinate. " +
			"Two tests because the rule is a range: one proves the outside is refused, the other that MinInt32 and MaxInt32 are still accepted. Without the second, a decoder that rejected every origin would satisfy the first.",
	},
	{
		Name: "a structure has one envelope", Category: Presence, Enforce: Decoded,
		Rules: []string{"658096bb", "7b480f2f"},
		Tests: []string{"TestStructureOriginExtremes", "TestRejectsStructureEnvelopeViolations"},
		Note:  "The envelope is the header flags, the empty settings blob, the empty biome palette, the size components and the origin range. The size and origin bounds are stated in §6's layout table rather than a sentence of their own, and a round trip of a legal structure cannot show either going.",
	},
	{
		Name: "cell padding is air", Category: Normalisation, Enforce: WriterOnly,
		Rules: []string{"c176f73b"},
		Tests: []string{"TestMaxLayerCellPadding"},
		Note:  "The box need not be a multiple of 16, so edge cells have padding that is not part of the structure. A reader could in principle check it, but padding is outside the structure by definition, so a file carrying it decodes to the same structure either way. Verified by re-encoding.",
	},

	// -- Limits and writer obligations (§8) -------------------------------
	{
		Name: "decoders enforce the limits", Category: Bound, Enforce: Decoded,
		Rules: []string{"398afbcf", "bcbf4d91"},
		Tests: []string{"TestNBTValidatorRejectsHostileLengths", "TestRejectsOverLimitCounts"},
		Note:  "The reference values are pinned as numbers, not only as constants with comments, and every count that goes through the shared bound is offered its ceiling and one past it over a body long enough that truncation cannot be the reason it is refused. The NBT fixtures reach none of this: their blobs are all short, so they fail for running out of bytes.",
	},
	{
		Name: "writers refuse what their readers reject", Category: Bound, Enforce: WriterOnly,
		Rules: []string{"126cf2d4"},
		Tests: []string{"TestSetMetaChecksAggregateFrame", "TestRejectedStoreRollsBackEveryPath"},
		Note: "Aggregate ceilings, not only per-field ones: a body of legal blobs can still pass the body limit. " +
			"Missing evidence: everything. The rule is about content a writer declined to emit, so the files it governs are the ones that do not exist. A file that does exist either satisfies the reader's rules or does not, and that verdict says nothing about whether its writer would have refused it. The only way to check this rule is to offer a writer input its reader rejects and require the write to fail, which is what both tests do.",
	},
	{
		Name: "decoders never panic", Category: Integrity, Enforce: Decoded,
		Rules: []string{"43ff86c5"},
		Tests: []string{"FuzzReadWorld", "FuzzReadStructure", "FuzzOpenIndexed", "FuzzNBTStability"},
		Note:  "The only entry whose control is not a fixture. A plain `go test` runs the seed corpora, which are the inputs already known to be safe, so disabling a bound leaves them green; what shows the property is real is that the same edits panic the targeted tests instead -- deleting the structure cell ceiling makes TestRejectsStructureCellOverflow panic in make. The evidence this entry needs is a long fuzzing session rather than a red test, and it is not something a fixture can stand in for.",
	},
	// -- Specification review (round 23) ---------------------------------
	{
		Name: "the hash seed is zero", Category: Normalisation, Enforce: Decoded,
		Rules: []string{},
		Tests: []string{"TestHashSeedIsZero", "TestHashSeedIsUsedInProduction"},
		Note:  "xxHash64 takes a seed and nothing else in the format implies which. An implementation that guesses differently agrees with this one on nothing.",
	},
	{
		Name: "the biome palette order is defined", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"33a5a48c"},
		Tests: []string{"TestBiomePaletteOrder"},
		Note:  "The sentence says writer-only in its own text and this entry said decoded, because it also claimed the uniqueness rule below, which is a reader's. What a reader would need is the reference counts the order is built from, and a biome file does not carry them: §4.7 counts a section before deciding to elide it, so the sections that decided the order are exactly the ones absent from the bytes. Verified by re-encoding.",
	},
	{
		Name: "biome palette entries are unique", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"230b214c"},
		Tests: []string{"TestRejectsDuplicatePaletteEntries"},
		Note:  "Held apart from the order above because the two have different enforcers: a repeated name is on the wire and a reader refuses it, while the order it sits in is not checkable at all.",
	},
	{
		Name: "dropped layers do not count", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"cd420726", "f61eb7d5"},
		Tests: []string{"TestDroppedAirLayersDoNotCount", "TestPaletteCountsOccurrencesNotBlobs"},
		Note: "A layer that never reaches the file contributes nothing to the palette order. " +
			"Missing evidence: the dropped layers. §4.3 requires trailing all-air layers to be absent, so a reader sees a record that never mentions them and cannot tell one that dropped them before counting from one that counted them and dropped them after. The two produce palettes in different orders and both are legal-looking files. Verified by re-encoding.",
	},
	{
		Name: "blob ids follow the field order", Category: Ordering, Enforce: Decoded,
		Rules: []string{"078b2b7d"},
		Tests: []string{"TestDedup", "TestReaderEnforcesBlobFirstUseOrder", "TestDecodersAgreeOnValidity"},
		Note: "The whole dedup table's identity depends on the assignment sequence, which is otherwise only deducible from the record layout. " +
			"Only the reader half has a control. The writer half cannot have one: an id is assigned and written by the same expression (blobTable.add inside the record's blob sink), so there is no edit that makes a writer assign a different id without also emitting that id, and any such writer produces a file its own reader refuses.",
	},
	{
		Name: "structure cells are computed in 64 bits", Category: Bound, Enforce: Decoded,
		Rules: []string{"0b740420"},
		Tests: []string{"TestRejectsStructureCellOverflow"},
		Note:  "Each axis alone may reach 1048576, so the rounding overflows a 32-bit value near the top of its range and the product far below it. A per-axis fixture cannot reach either: the sizes that overflow the product are all individually legal, and one of them makes a 32-bit multiply land on exactly zero, which a truncating reader would accept as an empty structure.",
	},
	{
		Name: "zstd frames are bounded", Category: Bound, Enforce: Decoded,
		Rules: []string{"8745186e", "7b6bfb2d"},
		Tests: []string{"TestRejectsOversizedZstdWindow"},
		Note: "The window ceiling bounds the memory a decode needs; the decoded-size ceilings bound what comes out, and a frame can sit far inside the window and still decompress past them, which is how a few hundred bytes demand a large buffer. Both halves have a fixture -- the second did not, so the decoded-size ceiling could be raised to a terabyte and the suite stayed green. " +
			"§2.5 used to carry a second rule here, that a frame declare its content size, which nothing enforced; it was struck rather than enforced because the reference encoder omits the field below a few hundred bytes and most frames an indexed file holds are smaller than that.",
	},
	{
		Name: "stats fields are optional but typed", Category: Presence, Enforce: Decoded,
		Rules: []string{"e23c42de", "fa104a93"},
		Tests: []string{"TestStatsMissingKeyAccepted", "TestStatsPreservesUnknownKeys", "TestRejectsStatsSchemaViolations"},
	},
	{
		Name: "enforcement is stated for every rule", Category: Presence, Enforce: WriterOnly,
		Rules: []string{"0f2361c9"},
		Tests: []string{"TestEveryInvariantNamesALiveTest"},
		Note: "The harness itself: every entry in this table must say whether readers reject violations, so a rule nobody can check on read cannot look like one that is checked. " +
			"Missing evidence: there is no file to look at. The rule constrains this table and the specification, not any sequence of bytes, so 'a reader cannot check it' is true in the strong sense that there is nothing for a reader to check. The enforcer is the harness, which fails when an entry leaves the field empty.",
	},
	{
		Name: "collection keys are unique", Category: Presence, Enforce: Decoded,
		Rules: []string{"c5770076", "0528d6ba"},
		Tests: []string{"TestRejectsDuplicateCollectionEntries", "TestStructureRejectsBlockEntityOutsideBox", "TestDecodersAgreeOnValidity", "TestStructureWriterRefusesDuplicateBlockEntities"},
		Note:  "The orders of §4.8 are total only because their keys are unique, so uniqueness is a rule rather than an assumption. Both readers enforce it through the strict ascent of the order itself: a sequence whose consecutive keys all ascend repeats nothing, and a seen-set that duplicated the rule could not fail. The structure half needs its own reader fixture and a writer one besides, because a writer that emits two entries at one position produces a file this package refuses to read back. It also makes the NBT tie-break §4.8 names for structure block entities unreachable: two entries can only tie on position, which is already refused.",
	},
	{
		Name: "StoreLight matches its content", Category: Omission, Enforce: Decoded,
		Rules: []string{"e0479882"},
		Tests: []string{"TestRejectsStoreLightWithoutLight"},
	},
	{
		Name: "empty palette segments are not written", Category: Presence, Enforce: Decoded,
		Rules: []string{"2ed66e73"},
		Tests: []string{"TestRejectsEmptyPaletteSegment"},
		Note:  "A segment with no entries is pure garbage two writers could differ on. The fixture is a hand-built file, because the writer never emits one and there is therefore nothing to patch. It has to name a segment of each kind: the two lists are decoded by different functions in different loops. This entry used to name the duplicate-reference test, which is a different sentence about a different amplifier and passes whatever this rule does.",
	},
	{
		Name: "decoders bound the result, not only the input", Category: Bound, Enforce: Decoded,
		Rules: []string{"70802cc1"},
		Tests: []string{"TestBoundsDecodedStorages", "TestBoundsDecodedNBTContainers", "TestBoundsCheckpointChain"},
		Note: "Several declared values cost far more to decode than to write: a blob reference is one byte and a live storage, a TAG_End inside a list of compounds is one byte and a whole map, an eleven-byte chunk record is a whole column, a 44-byte footer names a frame that may decompress to 512 MiB. Bounding the count against the remaining bytes does not bound any of those. " +
			"The storage and NBT ceilings both have a fixture that reaches them. The checkpoint-chain ceiling does not and cannot have one of the same kind: the walk terminates without it (a seen-set rules out cycles), so it caps work rather than deciding validity, and no input separates a file it refuses from one it accepts. TestBoundsCheckpointChain therefore only shows an ordinary chain is still walked, which is the half a wrong ceiling would break. The three sentences that spell out particular ceilings — NBT containers, columns, recovery work — are separate entries below.",
	},
	{
		Name: "the NBT container budget charges nested compounds", Category: Bound, Enforce: Decoded,
		Rules: []string{"6fbfdbc2"},
		Tests: []string{"TestHostileNBTContainerBudget", "TestNBTWriterHoldsTheContainerBudget"},
		Note: "The ceiling was stated in §8 from the start and the accounting only ever implemented half of it: a list's compound elements were charged and a compound's compound fields were not, so a blob of 2,097,152 sibling compounds — twice the ceiling, 14,680,068 bytes, inside the 16 MiB blob limit — was accepted and decoded into 461 MiB allocated and 265 MiB retained. " +
			"Two tests because the rule has a writer half. The reader's metadata blobs are revalidated on write, so those were covered by accident, but a block entity's or an entity's NBT went to the wire straight from the marshaller; the marshaller now keeps the same count, charged in the same places, so the writer cannot emit a blob its own reader refuses.",
	},
	{
		Name: "the column ceiling bounds both modes", Category: Bound, Enforce: Decoded,
		Rules: []string{"2e70d107"},
		Tests: []string{"TestHostileDecodedColumnCeiling", "TestRejectsOverLimitCounts"},
		Note: "§8 bounded decoded storages, NBT containers and the recovery chain and left columns at the width of the count field. That is not a bound: an eleven-byte chunk record need declare no section present, so a legal 1,161-byte file decoded into 1,048,576 columns and 1.04 GiB retained, and 2^32-1 permitted forty-eight thousand times as much before the 512 MiB body ceiling bound instead. " +
			"The two modes now share one number, which is the number an indexed directory already had. Its writer half needs no separate test: the encoder's own check is against the same constant, so the two cannot disagree.",
	},
	{
		Name: "recovery is bounded by total work", Category: Bound, Enforce: Decoded,
		Rules: []string{"56fab33d"},
		Tests: []string{"TestRecoveryWorkIsBounded", "TestHostileCheckpointReplay"},
		Note: "The chain limit and the directory limit bound two factors of one product and the product is what costs: 1, 4 and 16 forged footers over a directory at the entry ceiling measured 4.1 s, 14.9 s and 53.2 s from a file of about 9.5 KB, extrapolating to roughly fourteen minutes at the 256-candidate limit. Forging a candidate is free, since a footer's hash is xxHash64 over bytes its author controls. " +
			"Memoising by directory reference was considered and rejected: it collapses the case where every footer names one directory and an attacker pays about 200 bytes per distinct frame to evade it, so it buys a number rather than a bound. " +
			"The budget is spent across candidates, which is what makes it a bound on the product; the enforcement test lowers it, because reaching the shipped value means parsing sixteen million entries. The value itself is pinned by TestRejectsOverLimitCounts.",
	},
	{
		Name: "positions lie inside the declared span", Category: Bound, Enforce: Decoded,
		Rules: []string{"1870ce37"},
		Tests: []string{"TestRejectsOutOfSpanPositions"},
		Note:  "The span is validated; its contents were not. A block-entity Y outside it is a coordinate the caller's own array cannot address.",
	},
	{
		Name: "a caller's ceiling is policy, and tightens only", Category: Bound, Enforce: Decoded,
		Rules: []string{"781a9694", "80e86ca1"},
		Tests: []string{
			"TestDecodeBudgetDefaultChangesNothing",
			"TestDecodeBudgetCeilingIsUnreachableByDefault",
			"TestDecodeBudgetRefusesByPolicyNotValidity",
			"TestDecodeBudgetChargesColumnsWithNoStorages",
			"TestDecodeBudgetReachesEveryReader",
			"TestDecodeBudgetClampsUpwardOnly",
			"TestDecodeBudgetSentinelIsNotCorrupt",
			"TestIndexedDecodeBudgetIsPerHandle",
			"TestIndexedDecodeBudgetChargesTheRecordColumn",
			"TestIndexedDecodeBudgetDoesNotFallBack",
		},
		Note: "The one entry in this table that is not about which files are valid. §8's ceilings are set at what the format can represent, and a legal 1,161-byte file still decodes into 1.12 GiB, so no single constant serves both a server opening maps it did not write and an operator storing a genuine four-million-column world. MaxDecodedBytes hands that choice to the caller. " +
			"It is listed here because the two ways of getting it wrong are both format changes. Refusing under a caller's ceiling and reporting it as invalidity would teach a second implementation that the number is a validity rule, and it would then refuse conforming files and blame the file; ErrDecodeBudget therefore does not wrap ErrCorrupt, which is the sole documented exception to §8's convention. Letting a caller raise the ceiling past §8's would make this reader accept what a conforming reader must refuse, so the value is clamped downward and only downward. " +
			"The default has to be exactly the old behaviour, and is proved so twice: by sweeping every golden and every conformance vector for an unchanged verdict, reason and ContentHash, and by arithmetic — the default ceiling is set above the most §8 permits any decode to cost, so it cannot fire on a conforming file rather than merely not firing on the fixtures there are. " +
			"Enforce is Decoded because a reader is what applies it, but the enforcement is of the caller's policy and not of the format: no file is invalid for exceeding it.",
	},
}
