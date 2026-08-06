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

// Invariant is one canonicality rule.
type Invariant struct {
	// Name is a short handle used in failures and commit messages.
	Name string
	// Category is what kind of rule this is.
	Category Category
	// Rules are the ids of the format.md sentences this covers.
	Rules []string
	// Test names the test function that enforces it.
	Test string
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
		Name: "varints are minimal", Category: Normalisation,
		Rules: []string{"b5904bd9", "b970a5c4"},
		Test:  "TestRejectsNonMinimalVarints",
		Note:  "An overlong encoding is a second spelling of one number.",
	},
	{
		Name: "strings are bounded UTF-8", Category: Normalisation,
		Rules: []string{"ab5dc87f"},
		Test:  "TestRejectsNonUTF8Strings",
		Note:  "Palettes order strings bytewise, so arbitrary bytes would order differently under an implementation that decodes before comparing.",
	},
	{
		Name: "blobs are bounded", Category: Bound,
		Rules: []string{"f930c4ef"},
		Test:  "TestNBTValidatorRejectsHostileLengths",
	},
	{
		Name: "bitset padding is zero", Category: Normalisation,
		Rules: []string{"cf8e4eb4"},
		Test:  "TestRejectsNonCanonicalBlob",
		Note:  "Bits above the count carry no meaning, so a set one is a second encoding.",
	},
	{
		Name: "NBT compounds are canonical", Category: Ordering,
		Rules: []string{"2811e0b4", "e01f257a"},
		Test:  "TestEmptyNBTListCanonical",
		Note:  "Unique keys in ascending order is what lets independent writers agree.",
	},
	{
		Name: "array tags are distinct from lists", Category: Normalisation,
		Rules: []string{"10bba397"},
		Test:  "TestOpaqueNBTArraysSurvive",
		Note:  "Bedrock stores UUIDs as int arrays; folding them into lists is lossy.",
	},
	{
		Name: "metadata field tags are exact", Category: Presence,
		Rules: []string{"232fe73e", "4a44a61b", "f1047fc4", "e6b20330", "5bb2554d", "bf4e7d6b"},
		Test:  "TestRejectsMetaSchemaViolations",
	},

	// -- Header and container (§2) ---------------------------------------
	{
		Name: "unknown versions and flags are rejected", Category: Presence,
		Rules: []string{"a4590032", "648be581"},
		Test:  "TestRejectsReservedFlags",
		Note:  "Ignoring a reserved bit spends it: an old reader could not tell a future file needs a feature it lacks.",
	},
	{
		Name: "the dictionary bit stays reserved", Category: Presence,
		Rules: []string{"2b1d7218"},
		Test:  "TestRejectsReservedFlags",
	},
	{
		Name: "the default biome reference needs its flag", Category: Presence,
		Rules: []string{"6dd8dd9c"},
		Test:  "TestRejectsReservedFlags",
	},
	{
		Name: "kind and mode pairs are enumerated", Category: Presence,
		Rules: []string{"62b04284"},
		Test:  "TestRejectsIndexedStructureKind",
		Note:  "A structure is always solid, so an indexed structure names a layout that does not exist.",
	},
	{
		Name: "the dimension field is enumerated", Category: Presence,
		Rules: []string{"2fcb9dbf"},
		Test:  "TestRejectsReservedDimension",
		Note:  "A world file holds one dimension and nothing else in the file says which.",
	},
	{
		Name: "structures have no dimension", Category: Presence,
		Rules: []string{"2ffce926"},
		Test:  "TestStructureLeavesDimensionBitsZero",
	},
	{
		Name: "solid footers carry no indexed words", Category: Presence,
		Rules: []string{"af37ce22"},
		Test:  "TestSolidFooterMustBeZero",
	},

	// -- Palettes and blobs (§3) -----------------------------------------
	{
		Name: "the palette order is defined on encoded bytes", Category: Ordering,
		Rules: []string{"c06f9652"},
		Test:  "TestPaletteOrderFollowsEncodedBytes",
		Note:  "The tie-break is the entry's own bytes, so no implementation has to agree on a string form first. This is the rule that decides every palette index and therefore every section blob.",
	},
	{
		Name: "state properties are ordered and unique", Category: Ordering,
		Rules: []string{"5ed7d87f"},
		Test:  "TestRejectsUnorderedStateProperties",
		Note:  "A repeated key is worse than an out-of-order one: the later value silently wins, so two files decode to one state.",
	},
	{
		Name: "palette entries are unique", Category: Normalisation,
		Rules: []string{"afea490c", "ddf5c7be", "22ececda"},
		Test:  "TestPaletteMergesIdenticalEntries",
		Note:  "Two entries that encode identically at one version have nothing to order them by.",
	},
	{
		Name: "version overrides mean a different version", Category: Omission,
		Rules: []string{"2062adb1"},
		Test:  "TestVersionZeroRoundTrips",
		Note:  "An override equal to the palette's own version is redundant and grows a round trip that changed nothing.",
	},
	{
		Name: "reference counts predate deduplication", Category: Ordering,
		Rules: []string{"999897c0"},
		Test:  "TestPaletteCountsOccurrencesNotBlobs",
		Note:  "Counting distinct blobs instead reorders the palette exactly when deduplication succeeds.",
	},
	{
		Name: "section blobs are canonical", Category: Ordering,
		Rules: []string{"5ab4d3d6", "5240768e", "e72263a7", "9c7d6645", "34f2544c"},
		Test:  "TestRejectsNonCanonicalBlob",
	},
	{
		Name: "identical blobs share one table entry", Category: Normalisation,
		Rules: []string{"3a870a30"},
		Test:  "TestDedup",
	},

	// -- Chunk records (§4) ----------------------------------------------
	{
		Name: "biome counts predate elision", Category: Ordering,
		Rules: []string{"27ea7bc1"},
		Test:  "TestUnknownDefaultBiomePreserved",
		Note:  "Elision depends on the palette order, which depends on the counts: counting before elision is what stops that being circular.",
	},
	{
		Name: "the section span is addressable", Category: Bound,
		Rules: []string{},
		Test:  "TestRejectsUnaddressableSectionSpan",
		Note:  "sectionN bounds how tall a chunk is, not where it sits.",
	},
	{
		Name: "biome names are fully qualified", Category: Normalisation,
		Rules: []string{"249dada6"},
		Test:  "TestBiomeNamesAreNamespaced",
		Note:  "A bare name is not a stable identifier outside the registry that coined it.",
	},
	{
		Name: "chunk positions are unique and ascending", Category: Ordering,
		Rules: []string{"4e0f8a1e"},
		Test:  "TestRejectsDuplicateChunks",
	},
	{
		Name: "the section span is never trimmed", Category: Presence,
		Rules: []string{"fcf0a219"},
		Test:  "TestEmptyChunkKeepsFullSpan",
	},
	{
		Name: "layer counts are addressable", Category: Bound,
		Rules: []string{"af4b369f"},
		Test:  "TestRejectsUnaddressableLayerCount",
		Note:  "A 256th layer wedges any implementation that indexes layers with a byte.",
	},
	{
		Name: "trailing air layers go, internal ones stay", Category: Omission,
		Rules: []string{"a9ee3ac1", "673fb00d", "12bc11b2", "90e4364e"},
		Test:  "TestInternalAirLayerSurvives",
		Note:  "Layer numbers are semantic: dropping an internal one turns waterlogging into a liquid block.",
	},
	{
		Name: "light entries describe something", Category: Presence,
		Rules: []string{"929864f4"},
		Test:  "TestRejectsUndefinedLightFlags",
	},
	{
		Name: "UniqueID is stored verbatim", Category: Normalisation,
		Rules: []string{"8e623ce0"},
		Test:  "TestEntityIDZeroRoundTrips",
		Note:  "Zero is legal; rewriting it breaks encode, decode, encode.",
	},
	{
		Name: "the default biome flag is not optional", Category: Omission,
		Rules: []string{"13221d37", "7984f33b"},
		Test:  "TestUnknownDefaultBiomePreserved",
		Note:  "Declining the flag is a second encoding of the same world.",
	},
	{
		Name: "the biome fallback is version stable", Category: Normalisation,
		Rules: []string{"d3997566"},
		Test:  "TestAbsentBiomeFallbackIsVersionStable",
		Note:  "A numeric id is a property of the running game version, so naming one would let a single file decode to different biomes on two runtimes.",
	},
	{
		Name: "elided biome sections keep their names", Category: Omission,
		Rules: []string{"ff694fe9"},
		Test:  "TestUnknownDefaultBiomePreserved",
	},
	{
		Name: "collections are totally ordered", Category: Ordering,
		Rules: []string{"920faee4"},
		Test:  "TestCollectionTiesUseWrittenBytes",
		Note:  "Ties break on the bytes that get written, not on the caller's value.",
	},
	{
		Name: "stats keys are extensible", Category: Presence,
		Rules: []string{"8578b8c5"},
		Test:  "TestStatsFlag",
	},

	// -- Indexed mode (§5) -----------------------------------------------
	{
		Name: "absent frame references are all zero", Category: Normalisation,
		Rules: []string{"58866f01"},
		Test:  "TestIndexedDirectoryPrologueAuthoritative",
	},
	{
		Name: "the directory prologue is authoritative", Category: Integrity,
		Rules: []string{"cf48aa12"},
		Test:  "TestIndexedRecoversDamagedKindAndMode",
	},
	{
		Name: "recovered writers hash the rebuilt header", Category: Integrity,
		Rules: []string{"5befe4df"},
		Test:  "TestRecoveredHeaderCheckpointVerifies",
	},
	{
		Name: "the directory and dictionary skip the dictionary", Category: Presence,
		Rules: []string{"7c4f5f2c"},
		Test:  "TestIndexedDictionary",
	},
	{
		Name: "indexed files claim only what they can hold", Category: Presence,
		Rules: []string{"d2702006"},
		Test:  "TestIndexedRejectsSolidOnlyFlags",
		Note:  "Indexed mode has no stats field and elides no biome sections.",
	},
	{
		Name: "a dictionary needs compression", Category: Presence,
		Rules: []string{"fd0cd567"},
		Test:  "TestIndexedRejectsDictionaryWhenUncompressed",
	},
	{
		Name: "palette limits are cumulative", Category: Bound,
		Rules: []string{"ba81d481", "1983135b"},
		Test:  "TestIndexedSegmentVersioning",
	},

	// -- Structures (§6) --------------------------------------------------
	{
		Name: "a structure has one envelope", Category: Presence,
		Rules: []string{"5789390a", "7b480f2f"},
		Test:  "TestStructureOriginExtremes",
	},
	{
		Name: "cell padding is air", Category: Normalisation,
		Rules: []string{"c176f73b"},
		Test:  "TestMaxLayerCellPadding",
		Note:  "The box need not be a multiple of 16, so edge cells have padding that is not part of the structure.",
	},

	// -- Limits and writer obligations (§8) -------------------------------
	{
		Name: "decoders enforce the limits", Category: Bound,
		Rules: []string{"398afbcf", "bcbf4d91"},
		Test:  "TestNBTValidatorRejectsHostileLengths",
	},
	{
		Name: "writers refuse what their readers reject", Category: Bound,
		Rules: []string{"126cf2d4"},
		Test:  "TestIndexedRejectsOversizedMeta",
		Note:  "Aggregate ceilings, not only per-field ones: a body of legal blobs can still pass the body limit.",
	},
	{
		Name: "decoders never panic", Category: Integrity,
		Rules: []string{"43ff86c5"},
		Test:  "FuzzReadWorld",
	},
}
