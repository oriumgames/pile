# Filesystem behaviour: what it does, and whether that was on purpose

FREEZE.md asks for four things to be deliberate rather than accidental: path
traversal, symlinks on atomic rename, permission bits, and temp-file naming.
This is the audit. Every decision below is pinned by a test in
`fsbehaviour_test.go`, `cmd/pile/staging_test.go` or `format/durability_test.go`,
because a decision recorded only in prose is one that changes the next time
somebody edits the line.

Nothing here changes a byte a writer produces or a file a reader accepts.

## 1. Where files are created

`createExclusive` (`clone.go`) removes any stale file at the staging name and
then opens with `O_WRONLY|O_CREATE|O_EXCL`. `os.Create` is `…|O_TRUNC`, which
follows a symlink at the target: in a directory another user can write to, a
predictable `.tmp` name can be pre-created as a link pointing anywhere this
process can write, and the save goes through it. `O_EXCL` refuses to follow one
— including a dangling one, which is the interesting case, because following it
would *create* the target.

Removing first is deliberate and is not a hole: `unlink` on a symlink removes
the link, not its target, and the `O_EXCL` open that follows cannot be
redirected by anything that appears in between (it would fail with `EEXIST`
instead).

**Audited: every path that creates a file.**

| path | staging name | creates with | test |
|---|---|---|---|
| `Provider.Save` / `SaveAs` (`save.go:writeFile`) | `<dim>.pile.tmp` | `createExclusive` | `TestStagedNamesAreNeverWrittenThrough/Provider.Save` |
| `WorldFiles.Write` (`worldfiles.go:writeDimTemp`), solid | `<dim>.pile.tmp` | `createExclusive` | `…/WorldFiles.Write` |
| `WorldFiles.Write`, indexed | `<dim>.pile.tmp` | `format.CreateIndexed`, `O_EXCL` | `…/WorldFiles.Write` |
| `Structure.Save` (`structure.go`) | `<path>.tmp` | `createExclusive` | `…/Structure.Save` |
| `Provider.Snapshot` / `WorldFiles.Backup` (`snapshot.go:copyFile`) | the destination itself | `createExclusive` | — |
| `Provider.Rollback` (`snapshot.go`) | `<dim>.pile.rollback` | `createExclusive` | `…/Provider.Rollback` |
| `format.CreateIndexed` | — | `O_RDWR\|O_CREATE\|O_EXCL` | `TestCreateIndexedRefusesAnExistingPath` |
| `IndexedWorld.Compact` | `<path>.compact` | removes, then `CreateIndexed` | `…/IndexedWorld.Compact` |

**Found: the command-line tools were still using `os.Create`**, at three sites
that stage a predictable name beside a world file and then rename over it —
`cmd/pile/tools.go` (`<file>.upgrade`), `cmd/pile/maintain.go` (`<file>.mode`)
and `cmd/pile/move.go` (`<file>.tmp`). The exclusive staging that was added to
the library never reached them. `cmd/pile/staging.go` now holds `createStaged`,
the same function, and the three sites use it. It is a copy rather than an
import because the library's is unexported and `cmd/pile` is package `main`;
exporting it would have added public API to a library whose surface is about to
be reviewed as a surface.

`cmd/pile/render.go` still uses `os.Create`, deliberately: its argument is an
output path the user named on the command line, not a staging name this process
invented, and truncating a file the user asked to be written is what the user
asked for.

Controls: `F1` (turn `createExclusive` back into `os.Create`) turns four of the
five staging subtests red plus `TestStagingRefusesAnExistingPath`; `F1b`
(`O_EXCL` → `O_TRUNC` in `CreateIndexed`) turns `TestCreateIndexedRefusesAnExistingPath`
red; `F5` does the same for `cmd/pile`.

## 2. Symlinks at the destination

`rename(2)` replaces the link, it does not follow it. So a save cannot be
redirected by planting a symlink at the world file's own name:
`TestSaveReplacesASymlinkDestination` plants a *dangling* one, so following it
would have to create the target, and requires the target still not to exist
afterwards.

The consequence, which is a behaviour change to the operator and is deliberate:
**a dimension file that was symlinked onto another disk becomes a real file at
its original location on the first solid-mode save.** The alternative — resolve
the link and rename onto the resolved path — would give up the property above,
which is worth more.

**Append mode is the other way round, and that is also deliberate.** An indexed
world is appended to in place; that is the point of the mode. The path is
opened `O_RDWR`, so a symlink there is followed like any other, and the
symlinked file keeps working across saves. Refusing symlinked world files would
break the ordinary reason to have one. Planting a symlink in a world directory
already requires write access to that directory, which is enough to replace the
world file outright. `TestAppendModeWritesThroughASymlinkedDimensionFile` pins
it so the asymmetry is a decision rather than a surprise.

## 3. Path traversal

Caller-supplied strings that become path components:

- **Snapshot names.** The only one. `validateSnapshotName` rejects the empty
  string, `.`, `..`, and any name containing `/` or `\`, and all three of
  `Snapshot`, `Rollback` and `DeleteSnapshot` call it.
  `TestSnapshotNamesCannotEscape` drives ten shapes through all three.
  Control `F3` disables the check: every operation accepts every shape, and
  `DeleteSnapshot("../../../../etc")` — which is `os.RemoveAll` on a
  caller-chosen path — deleted the test's own temporary directory out from
  under it. That is what the check is worth.
- **Dimension file names** are generated, never taken: `DimPath` builds
  `overworld.pile`, `nether.pile`, `end.pile` or `dim<id>.pile` from a
  registered dimension's integer ID.
- **Rollback's source entries** come from `os.ReadDir`, whose `Name()` is
  always a single path element.
- **`worldDimensions`** globs `dim*.pile` inside the world directory and parses
  the id with `Sscanf`; a file whose id names an unregistered dimension is an
  error rather than something skipped, so its content cannot be quietly
  overwritten by regeneration.
- **`LoadStructure`, `Structure.Save`, `Open`, `SaveAs`** take whole paths from
  the caller by design. A library that is handed a path writes to that path.

## 4. Permission bits

Files are staged `0644` and directories created `0755`, both less the process
umask. That is the conventional answer for a server's data directory: readable
by the operator's tooling, writable only by the owner. It is now asserted
rather than assumed — `TestCreatedFilesAndDirectoriesHaveTheIntendedMode`
checks the dimension file, the `snapshots` directory, a snapshot directory and
a snapshot's copy of a dimension file, each against the constant less the
umask read from the running process.

**Found: an atomic replace widened the destination's permissions.** A rename
swaps the inode, so the file that lands carries the staging mode whatever the
one it replaced had: a world an operator had deliberately closed to `0600`
became world-readable on its first save, and nothing said so. `preserveMode`
(`clone.go`, and a copy in `cmd/pile/staging.go`) now copies an existing
regular destination's permission bits onto the staged file before the rename.
It is applied at every replace: `save.go:writeFile`, `worldfiles.go:Write`,
`snapshot.go:Rollback`, `structure.go:Save`, `format/indexed.go:Compact`, and
the three `cmd/pile` sites. A destination that does not exist keeps the staging
mode; one that is not a regular file is left alone, since a symlink's own bits
mean nothing and the rename replaces the link rather than its target.

`TestSavePreservesAnExistingFilesMode` covers the solid save, indexed
compaction and a structure save; control `F2` turns it red.

Ownership is not preserved and cannot be: a process that is not root cannot
give a file away. A world file saved by a different user than the one that
created it changes owner. Nothing can be done about that at this layer and it
is recorded rather than fixed.

## 5. Temp-file naming, and concurrent processes

Staging names are fixed and derived from the destination: `.tmp`, `.rollback`,
`.rollbackold`, `.compact`, `.upgrade`, `.mode`. They are predictable on purpose
— a crashed run leaves exactly one file per destination, which the next run
removes, rather than an unbounded litter of unique names nobody will ever clean
up. Predictable is safe here because `O_EXCL` makes the name unusable as an
attack surface (§1), which is the property that would otherwise argue for
randomising it.

`.rollbackold` is the odd one and is not a staging file: it is the *existing*
dimension file, moved aside by `os.Rename` while a rollback installs the
snapshot's copy over it, and removed only once the restored world has been read
back. `Rollback` used to delete the current files outright and read the result
afterwards, so a snapshot directory holding something that is not a world
destroyed the world — see `SECURITY.md`, "The provider surface and the CLI",
finding 3. The window a crash can land in is the same one as before, and what it
leaves behind is strictly better: the world's own bytes are still on disk under
`.rollbackold` rather than gone. Nothing removes them automatically, on purpose;
a `.rollbackold` beside a missing dimension file is the operator's cue.

**What that does not buy is mutual exclusion between processes.** Because
`createExclusive` removes a stale staging file before creating, two processes
saving the same world directory at the same time will each delete the other's
temp file and each rename over the destination; the survivor is whichever
renamed last, and the loser's save is silently gone. `O_EXCL` is protection
against a pre-created path, not a lock.

**This is a stated assumption, not a defect that was fixed: a world directory
has one owner.** The provider serialises its own saves with `saveMu`, and
indexed files are held open `O_RDWR` for the provider's lifetime, but nothing
excludes a second process. Making it safe means a lock file, which is a
different piece of work with its own failure modes (stale locks, network
filesystems) and no bearing on the format. It is recorded here so that the next
person to read `createExclusive` does not mistake it for one.

`TestNoStagingFileSurvivesAFailedSave` covers the other half — that a save
which fails at the rename does not leave its staging file behind for the next
run to trip over. Control `F4` turns it red.

## 6. The destination as something other than a file

- **A directory.** `os.Rename` fails, the error is returned, the world stays
  dirty and the save is retryable: `TestCloseIsRetryable` and
  `TestNoStagingFileSurvivesAFailedSave`. Opening a world whose dimension file
  is a directory is an error naming the problem rather than a panic or a
  silently empty world: `TestOpenRefusesADimensionFileThatIsADirectory`.
- **A symlink.** §2.
- **A file owned by somebody else.** `rename(2)` needs write permission on the
  *directory*, not on the file, so a save over another user's file succeeds
  wherever the directory allows it (sticky-bit directories excepted, where the
  kernel refuses). That is POSIX and there is nothing to decide; the file that
  lands is owned by this process, which is why `preserveMode` copies the bits
  and cannot copy the owner.
- **A FIFO, device node or socket.** `preserveMode` leaves it alone (not a
  regular file) and the rename replaces it, the same as a symlink.

## Not covered

- **Windows.** The permission tests skip there, and `rename(2)`'s
  replace-in-place semantics are `MoveFileEx`'s on that platform, which Go's
  `os.Rename` uses with `MOVEFILE_REPLACE_EXISTING`. Nothing here has been run
  on Windows.
- **Network filesystems.** `fsync` on a directory, which `syncDir` relies on to
  make a rename durable, is not meaningful on every remote filesystem.
- **`TOCTOU` between `Lstat` and `Chmod` in `preserveMode`.** The window is
  between reading the destination's mode and setting it on a file this process
  created and holds the only reference to; the worst outcome is that the staged
  file gets a mode the destination had a moment ago.
- **Cross-process locking.** §5.
