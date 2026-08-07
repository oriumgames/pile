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
		Rules: []string{"27513eef"},
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
		Rules: []string{"10bba397"},
		Tests: []string{"TestOpaqueNBTArraysSurvive"},
		Note:  "Bedrock stores UUIDs as int arrays; folding them into lists is lossy.",
	},
	{
		Name: "metadata field tags are exact", Category: Presence, Enforce: Decoded,
		Rules: []string{"232fe73e", "4a44a61b", "f1047fc4", "e6b20330", "5bb2554d", "bf4e7d6b"},
		Tests: []string{"TestRejectsMetaSchemaViolations", "TestReaderEnforcesMetadataSchemas"},
	},

	// -- Header and container (§2) ---------------------------------------
	{
		Name: "unknown versions and flags are rejected", Category: Presence, Enforce: Decoded,
		Rules: []string{"a4590032", "648be581", "718f271d"},
		Tests: []string{"TestRejectsReservedFlags", "TestIndexedRejectsReservedFlags"},
		Note:  "Ignoring a reserved bit spends it: an old reader could not tell a future file needs a feature it lacks.",
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
		Tests: []string{"TestRejectsIndexedStructureKind", "TestIndexedRejectsStructureKindInDirectory"},
		Note:  "A structure is always solid, so an indexed structure names a layout that does not exist.",
	},
	{
		Name: "the dimension field is enumerated", Category: Presence, Enforce: Decoded,
		Rules: []string{"2fcb9dbf"},
		Tests: []string{"TestRejectsReservedDimension"},
		Note:  "A world file holds one dimension and nothing else in the file says which.",
	},
	{
		Name: "structures have no dimension", Category: Presence, Enforce: Decoded,
		Rules: []string{"2ffce926"},
		Tests: []string{"TestStructureLeavesDimensionBitsZero"},
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
		Note:  "The tie-break is the entry's own bytes, so no implementation has to agree on a string form first. This is the rule that decides every palette index and therefore every section blob.",
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
		Rules: []string{"2062adb1"},
		Tests: []string{"TestVersionZeroRoundTrips", "TestRejectsRedundantVersionOverride"},
		Note:  "An override equal to the palette's own version is redundant and grows a round trip that changed nothing.",
	},
	{
		Name: "reference counts predate deduplication", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{},
		Tests: []string{"TestPaletteCountsOccurrencesNotBlobs"},
		Note:  "Counting distinct blobs instead reorders the palette exactly when deduplication succeeds.",
	},
	{
		Name: "section blobs are canonical", Category: Ordering, Enforce: Decoded,
		Rules: []string{"5ab4d3d6", "5240768e", "e72263a7", "9c7d6645", "34f2544c"},
		Tests: []string{"TestRejectsNonCanonicalBlob", "TestRejectsOutOfRangePaletteIndex"},
		Note:  "The blob decoder checks the palette and the width; the index bytes are checked only where the section is applied, and each width has its own loop, so the two halves need separate fixtures.",
	},
	{
		Name: "identical blobs share one table entry", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"1e589410"},
		Tests: []string{"TestDedup", "TestRejectsBlobTableWaste", "TestRejectsUnreferencedBlob"},
	},

	// -- Chunk records (§4) ----------------------------------------------
	{
		Name: "biome counts predate elision", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"27ea7bc1"},
		Tests: []string{"TestBiomeCountsPrecedeElision"},
		Note:  "Elision depends on the palette order, which depends on the counts: counting before elision is what stops that being circular.",
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
		Name: "the section span is never trimmed", Category: Presence, Enforce: WriterOnly,
		Note:  "A reader cannot know the span a dimension intended, only the one the record declares, so a trimmed record is indistinguishable from an honest one. Verified by re-encoding.",
		Rules: []string{"fcf0a219"},
		Tests: []string{"TestEmptyChunkKeepsFullSpan"},
	},
	{
		Name: "layer counts are addressable", Category: Bound, Enforce: Decoded,
		Rules: []string{"af4b369f"},
		Tests: []string{"TestRejectsLayerCountInRecords", "TestRejectsLayerCountInCells"},
		Note:  "A 256th layer wedges any implementation that indexes layers with a byte. Placed where a record actually carries the count, so the rule survives a parser that stops consulting the shared bound.",
	},
	{
		Name: "trailing air layers go, internal ones stay", Category: Omission, Enforce: Decoded,
		Rules: []string{"a9ee3ac1", "673fb00d", "12bc11b2", "90e4364e"},
		Tests: []string{"TestInternalAirLayerSurvives", "TestTrailingAirLayersDropped", "TestStructureInternalAirLayerSurvives", "TestRejectsNonCanonicalSections"},
		Note:  "Layer numbers are semantic: dropping an internal one turns waterlogging into a liquid block.",
	},
	{
		Name: "light entries describe something", Category: Presence, Enforce: Decoded,
		Rules: []string{"929864f4"},
		Tests: []string{"TestRejectsLightEntryFlags"},
	},
	{
		Name: "UniqueID is stored verbatim", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"8e623ce0"},
		Tests: []string{"TestEntityIDZeroRoundTrips"},
		Note:  "Zero is legal; rewriting it breaks encode, decode, encode.",
	},
	{
		Name: "the default biome flag is not optional", Category: Omission, Enforce: WriterOnly,
		Rules: []string{"13221d37", "7984f33b"},
		Tests: []string{"TestUnknownDefaultBiomePreserved", "TestDefaultBiomeFlagIsSet"},
		Note:  "Declining the flag is a second encoding of the same world, but a file that declined it decodes to that world all the same and nothing in it says which sections were uniform before the writer chose. Verified by re-encoding.",
	},
	{
		Name: "the biome fallback is version stable", Category: Normalisation, Enforce: Decoded,
		Rules: []string{"d3997566"},
		Tests: []string{"TestAbsentBiomeFallbackIsVersionStable"},
		Note:  "A numeric id is a property of the running game version, so naming one would let a single file decode to different biomes on two runtimes.",
	},
	{
		Name: "elided biome sections keep their names", Category: Omission, Enforce: Decoded,
		Rules: []string{"ff694fe9"},
		Tests: []string{"TestUnknownDefaultBiomePreserved"},
	},
	{
		Name: "collections are totally ordered", Category: Ordering, Enforce: Decoded,
		Rules: []string{"920faee4"},
		Tests: []string{"TestCollectionTiesUseWrittenBytes", "TestTiedTicksAndStructureCollections", "TestReaderEnforcesCollectionOrder"},
		Note:  "Ties break on the bytes that get written, not on the caller's value.",
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
		Tests: []string{"TestIndexedRecoversDamagedKindAndMode"},
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
		Tests: []string{"TestRejectsDirectoryStorageMismatch"},
		Note:  "Defence in depth rather than an independently reachable rule: a real file cannot carry a compressed dictionary while claiming to be uncompressed without its directory frame tripping the storage-form rule first, which is what this test drives.",
	},
	{
		Name: "palette limits are cumulative", Category: Bound, Enforce: Decoded,
		Rules: []string{"ba81d481", "89ca9097"},
		Tests: []string{"TestRejectsDuplicateSegmentReference", "TestRejectsCumulativePaletteOverflow"},
		Note:  "Duplicate references and the frame-total bound have fixtures. The million-entry ceiling itself is reachable only by fuzzing, since no fixture builds a palette that large; that is stated rather than papered over with a test that would pass either way.",
	},

	// -- Structures (§6) --------------------------------------------------
	{
		Name: "a structure has one envelope", Category: Presence, Enforce: Decoded,
		Rules: []string{"5789390a", "7b480f2f"},
		Tests: []string{"TestStructureOriginExtremes", "TestRejectsStructureEnvelopeViolations"},
		Note:  "The envelope is the header flags, the three metadata blobs, the empty biome palette, the size components and the origin range. The size and origin bounds are stated in §6's layout table rather than a sentence of their own, and a round trip of a legal structure cannot show either going.",
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
	},
	{
		Name: "writers refuse what their readers reject", Category: Bound, Enforce: WriterOnly,
		Rules: []string{"126cf2d4"},
		Tests: []string{"TestSetMetaChecksAggregateFrame", "TestRejectedStoreRollsBackEveryPath"},
		Note:  "Aggregate ceilings, not only per-field ones: a body of legal blobs can still pass the body limit.",
	},
	{
		Name: "decoders never panic", Category: Integrity, Enforce: Decoded,
		Rules: []string{"43ff86c5"},
		Tests: []string{"FuzzReadWorld", "FuzzReadStructure", "FuzzOpenIndexed", "FuzzNBTStability"},
	},
	// -- Specification review (round 23) ---------------------------------
	{
		Name: "the hash seed is zero", Category: Normalisation, Enforce: Decoded,
		Rules: []string{},
		Tests: []string{"TestHashSeedIsZero", "TestHashSeedIsUsedInProduction"},
		Note:  "xxHash64 takes a seed and nothing else in the format implies which. An implementation that guesses differently agrees with this one on nothing.",
	},
	{
		Name: "the biome palette order is defined", Category: Ordering, Enforce: Decoded,
		Rules: []string{"33a5a48c", "230b214c"},
		Tests: []string{"TestBiomePaletteOrder", "TestRejectsDuplicatePaletteEntries"},
	},
	{
		Name: "dropped layers do not count", Category: Ordering, Enforce: WriterOnly,
		Rules: []string{"cd420726", "f61eb7d5"},
		Tests: []string{"TestDroppedAirLayersDoNotCount", "TestPaletteCountsOccurrencesNotBlobs"},
		Note:  "A layer that never reaches the file contributes nothing to the palette order.",
	},
	{
		Name: "blob ids follow the field order", Category: Ordering, Enforce: Decoded,
		Rules: []string{"078b2b7d"},
		Tests: []string{"TestDedup", "TestReaderEnforcesBlobFirstUseOrder"},
		Note:  "The whole dedup table's identity depends on the assignment sequence, which is otherwise only deducible from the record layout.",
	},
	{
		Name: "structure cells are computed in 64 bits", Category: Bound, Enforce: Decoded,
		Rules: []string{"0b740420"},
		Tests: []string{"TestRejectsOverLimitCounts"},
		Note:  "Each axis alone may reach 1048576, so the rounding overflows a 32-bit value near the top of its range and the product far below it.",
	},
	{
		Name: "zstd frames are bounded", Category: Bound, Enforce: Decoded,
		Rules: []string{"8745186e", "7b6bfb2d", "edd065ba"},
		Tests: []string{"TestRejectsOversizedZstdWindow"},
		Note:  "The decoded-size ceilings bound the output, not the memory needed to produce it.",
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
		Note:  "The harness itself: every entry in this table must say whether readers reject violations, so a rule nobody can check on read cannot look like one that is checked.",
	},
	{
		Name: "collection keys are unique", Category: Presence, Enforce: Decoded,
		Rules: []string{"c5770076", "0528d6ba"},
		Tests: []string{"TestRejectsDuplicateCollectionEntries", "TestStructureRejectsBlockEntityOutsideBox"},
		Note:  "The orders of §4.8 are total only because their keys are unique, so uniqueness is a rule rather than an assumption. In a chunk record the reader enforces it through the strict ascent of the order itself: a sequence whose consecutive keys all ascend repeats nothing, and a seen-set that duplicated the rule could not fail. A structure's block entities are covered only for the box they must lie in; nothing on either side of the format rejects two of them at one position.",
	},
	{
		Name: "StoreLight matches its content", Category: Omission, Enforce: Decoded,
		Rules: []string{"e0479882"},
		Tests: []string{"TestRejectsStoreLightWithoutLight"},
	},
	{
		Name: "empty palette segments are not written", Category: Presence, Enforce: Decoded,
		Rules: []string{"2ed66e73"},
		Tests: []string{"TestRejectsDuplicateSegmentReference"},
		Note:  "A segment with no entries is pure garbage two writers could differ on.",
	},
	{
		Name: "decoders bound the result, not only the input", Category: Bound, Enforce: Decoded,
		Rules: []string{"74948bd9"},
		Tests: []string{"TestBoundsDecodedStorages", "TestBoundsDecodedNBTContainers", "TestBoundsCheckpointChain"},
		Note:  "Several declared values cost far more to decode than to write: a blob reference is one byte and a live storage, a TAG_End inside a list of compounds is one byte and a whole map, a 44-byte footer names a frame that may decompress to 512 MiB. Bounding the count against the remaining bytes does not bound any of those.",
	},
	{
		Name: "positions lie inside the declared span", Category: Bound, Enforce: Decoded,
		Rules: []string{"acfe3166"},
		Tests: []string{"TestRejectsOutOfSpanPositions"},
		Note:  "The span is validated; its contents were not. A block-entity Y outside it is a coordinate the caller's own array cannot address.",
	},
}
