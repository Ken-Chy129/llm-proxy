"""Private, crash-aware local journal for a single provisioning run (POSIX)."""

import json
import os
from pathlib import Path
import stat
import tempfile


class StateError(Exception):
    pass


class Journal:
    def __init__(self, directory):
        self.directory = Path(directory).absolute()
        self.data = {}
        self.lock = None

    def __enter__(self):
        try:
            import fcntl
            if self.directory.is_symlink():
                raise StateError("state directory must not be a symlink")
            # macOS /var and /tmp are system symlinks; canonicalize the parent.
            self.directory = self.directory.parent.resolve() / self.directory.name
            if not self.directory.exists():
                self.directory.mkdir(mode=0o700)  # parent must already exist
            info = self.directory.stat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise StateError("state directory must be owner-only (0700)")
            allowed = {"run.lock", "state.json", "credentials.json", "result.json"}
            if any(p.name not in allowed and not p.name.startswith(".pending-")
                   for p in self.directory.iterdir()):
                raise StateError("use a dedicated state directory, not an existing data directory")
            self.lock = os.open(self.directory / "run.lock",
                                os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.data = self.read("state.json") or {}
            if not isinstance(self.data, dict):
                raise StateError("state journal must be an object")
            if not self.data and any((self.directory / name).exists()
                                     for name in ("credentials.json", "result.json")):
                raise StateError("state journal missing but credentials/results exist; refusing a new run")
            return self
        except StateError:
            if self.lock is not None:
                os.close(self.lock)
                self.lock = None
            raise
        except (OSError, ValueError, ImportError):
            if self.lock is not None:
                os.close(self.lock)
                self.lock = None
            raise StateError("cannot open/lock private state directory; check path, permissions or another run") from None

    def __exit__(self, *_):
        if self.lock is not None:
            os.close(self.lock)

    def read(self, name):
        path = self.directory / name
        if not path.exists() and not path.is_symlink():
            return None
        try:
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(fd) as file:
                info = os.fstat(file.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid():
                    raise StateError("state/credential files must be owner-only regular files")
                return json.load(file)
        except (OSError, ValueError):
            raise StateError("cannot read private state file") from None

    def write(self, name, value):
        # Only code-owned basenames reach this method.
        fd, temporary = tempfile.mkstemp(prefix=".pending-", dir=self.directory)
        try:
            with os.fdopen(fd, "w") as file:
                json.dump(value, file, ensure_ascii=False, indent=2)
                file.write("\n")
                file.flush()
                os.fsync(file.fileno())
            os.replace(temporary, self.directory / name)
            directory_fd = os.open(self.directory, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def save(self):
        self.write("state.json", self.data)

    def begin(self, step):
        if self.data.get("pending"):
            raise StateError("previous write outcome unknown: " + self.data["pending"] +
                             "; inspect remote state before recovery; not retried")
        self.data["pending"] = step
        self.save()

    def finish(self, step, value):
        self.data[step] = value
        self.data.pop("pending", None)
        self.save()
