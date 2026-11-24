"""Drive the picker and chat through the same terminal as a user."""

import errno
import fcntl
import os
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import time


binary, turn, selected, answer, unfinished, partial, other, other_reply, follow, reply, after, after_reply = sys.argv[1:]
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 48, 300, 0, 0))
child = subprocess.Popen([binary, "resume"], stdin=slave, stdout=slave, stderr=slave,
                         start_new_session=True)
os.close(slave)

# Bubble Tea homes the cursor, paints changed rows, and positions the cursor
# after each alternate-screen frame. Count notices on the resulting screen so
# repeated redraws of one block cannot be mistaken for duplicate notices.
frame = re.compile(rb"\x1b\[H(.*?)\x1b\[\d+;\d*H", re.S)
escapes = re.compile(rb"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]")
pending = b""
rows = []


def expect(*markers):
    global pending, rows
    deadline = time.monotonic() + 25
    while True:
        match = frame.search(pending)
        if match:
            body = match[1].rsplit(b"\x1b[H", 1)[-1]
            pending = pending[match.end():]
            updated = []
            for i, raw in enumerate(body.split(b"\n")):
                if raw.strip(b"\r") or i >= len(rows):
                    updated.append(escapes.sub(b"", raw).decode("utf-8", "replace").strip())
                else:
                    updated.append(rows[i])
            rows = updated
            screen = "\n".join(rows)
            if all(marker in screen for marker in markers):
                return screen
            continue
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([master], [], [], remaining)[0]:
            break
        try:
            chunk = os.read(master, 65536)
        except OSError as err:
            if err.errno != errno.EIO:
                raise
            break
        if not chunk:
            break
        pending += chunk
    raise AssertionError(f"waiting for {markers!r}, exit={child.poll()}\n"
                         + "\n".join(rows) + f"\nlast bytes: {pending[-2000:]!r}")


def click_row(marker):
    row = next(i for i, text in enumerate(rows, 1) if marker in text)
    os.write(master, f"\x1b[<0;3;{row}M\x1b[<0;3;{row}m".encode())


def submit(text):
    os.write(master, text.encode() + b"\r")


try:
    expect("Resume a previous session", "› " + other, selected)
    os.write(master, b"\x1b[B")
    expect("› " + selected)
    os.write(master, b"\r")
    screen = expect(selected, answer, unfinished, partial, "Partial model response", "2 tool calls", "Ready", "Ask GSEA")
    assert other not in screen and other_reply not in screen, screen
    print(f"picker: {other} -> {selected}; TUI history: {answer}")

    assert "At session open" not in screen and "/recoveries" not in screen, screen
    click_row("2 tool calls")
    screen = expect("‹ Back", "ledger.txt", "Enter open")
    assert answer not in screen, screen
    click_row("ledger.txt")
    screen = expect("Esc back", "↑↓ scroll", "Verified ledger balance is 420.")
    assert answer not in screen, screen
    click_row("‹ Back")
    expect("Enter open", "ledger.txt")
    click_row("‹ Back")
    screen = expect("2 tool calls", answer, partial)
    assert "Verified ledger balance" not in screen and "‹ Back" not in screen, screen
    print("mouse: tool group -> recorded result -> list -> original chat")

    notice = "host executable changed"
    submit(follow)
    screen = expect(reply, "Ready")
    assert notice not in screen, screen

    submit("/recoveries")
    expect(turn, "unavailable", "requires the original GSEA executable")
    submit("/resume " + turn)
    screen = expect(notice, "Use the original GSEA executable", "Ready")
    assert screen.count(notice) == 1 and screen.count("Use the original GSEA executable") == 1, screen
    print(f"explicit /resume {turn}: {screen.count(notice)} host-change notice, Ready")

    submit(after)
    expect(after_reply, "Ready")
    os.write(master, b"\x04")  # Ctrl-D at the idle prompt.
    code = child.wait(timeout=15)
    if code != 0:
        tail = pending
        while select.select([master], [], [], 0)[0]:
            try:
                chunk = os.read(master, 65536)
            except OSError as err:
                if err.errno != errno.EIO:
                    raise
                break
            if not chunk:
                break
            tail += chunk
        raise AssertionError(f"gsea resume exited {code}: {escapes.sub(b'', tail)[-2000:]!r}")
finally:
    if child.poll() is None:
        os.killpg(child.pid, signal.SIGKILL)
        child.wait()
    os.close(master)
