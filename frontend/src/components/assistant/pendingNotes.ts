// pendingNotes remembers the assistant sessions whose answer still has to be
// posted as a run note. The store is per viewer, so it never reaches another
// browser. Every read and write sits in try/catch: a blocked store only loses
// the reminder, never the session.

export interface PendingNote {
  sessionId: string;
  runId: string;
  at: number;
}

const DAY_MS = 24 * 60 * 60 * 1000;

function key(subject: string): string {
  return `varroa-assistant-pending-notes:${subject}`;
}

function read(subject: string): PendingNote[] {
  try {
    const raw = window.localStorage.getItem(key(subject));
    const parsed = raw ? (JSON.parse(raw) as unknown) : [];
    return Array.isArray(parsed) ? (parsed as PendingNote[]) : [];
  } catch {
    return [];
  }
}

function write(subject: string, notes: PendingNote[]): void {
  try {
    window.localStorage.setItem(key(subject), JSON.stringify(notes));
  } catch {
    // A blocked store loses the reminder only.
  }
}

/** pendingNotes returns the store functions of one viewer. */
export function pendingNotes(subject: string) {
  return {
    list: (): PendingNote[] => read(subject),
    add(note: { sessionId: string; runId: string; at?: number }): void {
      const rest = read(subject).filter((n) => n.sessionId !== note.sessionId);
      write(subject, [...rest, { sessionId: note.sessionId, runId: note.runId, at: note.at ?? Date.now() }]);
    },
    remove(sessionId: string): void {
      write(subject, read(subject).filter((n) => n.sessionId !== sessionId));
    },
    /** prune drops the notes older than a day. */
    prune(now: number = Date.now()): void {
      write(subject, read(subject).filter((n) => now - n.at < DAY_MS));
    },
  };
}
