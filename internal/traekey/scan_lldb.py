"""lldb script: recover a Trae SQLCipher key from a live process.

Trae encrypts its ai-agent database with SQLCipher in raw-key mode and keeps no
copy of the key on disk: it exists only in the memory of the process that opened
the database. This script therefore reads the readable pages of that process,
collects the hex runs a key can hide in, and *proves* each candidate with
HMAC-SHA512 over page 1 of the database instead of reversing the derivation.

The method comes from Oh-My-Trae/trae-db-decrypt (MIT) and wechat-decrypt: in
raw-key mode the key material is one continuous hex string

    x'<64 hex chars = 32-byte key><32 hex chars = 16-byte salt>'

so a 64-character window inside any hex run is a candidate, and the HMAC of
page 1 says whether it is the right one. Nothing is decrypted here.

Inside lldb (after `command script import scan_lldb.py`):

    trae_scan

Environment:
    TRAE_DB       database to prove the key against; default is the first
                  Trae product database found under ~/Library/Application Support
    TRAE_KEY_OUT  where to write the result; default $TMPDIR/trae-key.json
"""

import glob
import hashlib
import hmac
import json
import os
import re
import struct

import lldb

PAGE_SZ = 4096
RESERVE_SZ = 80
CHUNK = 8 * 1024 * 1024
# A hex run long enough to hold a key, and one long enough to hold a key next to
# the 32-character salt that follows it.
TICK = re.compile(rb"[0-9a-fA-F]{60,200}")


def _default_db():
    base = os.path.expanduser("~/Library/Application Support")
    for product in ("Trae CN", "TRAE SOLO CN", "Trae"):
        path = os.path.join(base, product, "ModularData", "ai-agent", "database.db")
        if os.path.exists(path):
            return path
    return ""


def _db_path():
    path = os.environ.get("TRAE_DB") or _default_db()
    if not path or not os.path.exists(path):
        raise SystemExit("no Trae database found; set TRAE_DB to database.db")
    return path


def _out_path():
    return os.environ.get("TRAE_KEY_OUT") or os.path.join(
        os.environ.get("TMPDIR", "/tmp"), "trae-key.json"
    )


def _page1(db):
    with open(db, "rb") as f:
        page = f.read(PAGE_SZ)
    if len(page) != PAGE_SZ or page[:16] == b"SQLite format 3\x00":
        raise SystemExit("%s is not an encrypted SQLCipher database" % db)
    return page


def _verify(enc_key, page1):
    """True when enc_key is the key that signed page 1."""
    salt = page1[:16]
    mac_salt = bytes(b ^ 0x3A for b in salt)
    mac_key = hashlib.pbkdf2_hmac("sha512", enc_key, mac_salt, 2, dklen=32)
    # Page 1's MAC covers the page from byte 16 (the salt is not signed) up to
    # where the IV starts, followed by the little-endian page number.
    covered = page1[16 : PAGE_SZ - RESERVE_SZ + 16]
    stored = page1[PAGE_SZ - 64 : PAGE_SZ]
    digest = hmac.new(mac_key, covered, hashlib.sha512)
    digest.update(struct.pack("<I", 1))
    return digest.digest() == stored


def _candidates(blob, salt_hex, out):
    for tick in TICK.finditer(blob):
        run = tick.group().decode()
        for off in range(0, max(1, len(run) - 63), 2):
            candidate = run[off : off + 64]
            if len(candidate) < 64:
                break
            out.add(candidate)
            # The x'<key><salt>' form: keep the key that sits right before our
            # own salt, which is the one SQLCipher was handed.
            if len(run) >= off + 96 and run[off + 64 : off + 96].lower() == salt_hex:
                out.add(candidate)


def trae_scan(debugger, command, result, internal_dict):
    db = _db_path()
    page1 = _page1(db)
    salt_hex = page1[:16].hex()
    process = debugger.GetSelectedTarget().GetProcess()
    if not process.IsValid():
        result.PutCString("no process selected")
        return

    found = set()
    addr = 0
    scanned = 0
    while addr < 0x800000000000:
        region = lldb.SBMemoryRegionInfo()
        if not process.GetMemoryRegionInfo(addr, region).Success():
            break
        start, end = region.GetRegionBase(), region.GetRegionEnd()
        if end <= start:
            break
        if region.IsReadable() and not region.IsExecutable():
            pos = start
            while pos < end:
                err = lldb.SBError()
                buf = process.ReadMemory(pos, min(CHUNK, end - pos), err)
                if err.Success() and buf:
                    scanned += len(buf)
                    _candidates(buf, salt_hex, found)
                pos += CHUNK
        addr = end

    result.PutCString(
        "scanned %.0f MB, %d candidates, salt=%s" % (scanned / 1048576.0, len(found), salt_hex)
    )
    for candidate in found:
        if not _verify(bytes.fromhex(candidate), page1):
            continue
        out = _out_path()
        with open(out, "w") as f:
            json.dump({"db_path": db, "salt": salt_hex, "enc_key": candidate}, f, indent=2)
        result.PutCString("FOUND key=%s" % candidate)
        result.PutCString("saved -> %s" % out)
        return
    result.PutCString("no verified key in this process")


def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand(
        "command script add -f %s.trae_scan trae_scan" % __name__.rsplit(".", 1)[-1]
    )
