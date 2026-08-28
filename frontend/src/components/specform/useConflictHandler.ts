import { useState, useCallback } from "react";
import { ControllerConflictError, type ConflictInfo } from "../../api/client";

export interface ConflictState {
  conflicts: ConflictInfo[];
  showDialog: boolean;
}

/**
 * Shared hook for handling ControllerConflictError from updateController calls.
 * Returns state for the ConflictDialog plus wrappers for save and retry.
 */
export function useSaveWithConflict(onSave: (force: boolean) => Promise<void>) {
  const [conflict, setConflict] = useState<ConflictState>({
    conflicts: [],
    showDialog: false,
  });

  const save = useCallback(async () => {
    try {
      await onSave(false);
      setConflict({ conflicts: [], showDialog: false });
    } catch (e) {
      if (e instanceof ControllerConflictError) {
        setConflict({ conflicts: e.conflicts, showDialog: true });
      } else {
        throw e;
      }
    }
  }, [onSave]);

  const override = useCallback(async () => {
    try {
      await onSave(true);
      setConflict({ conflicts: [], showDialog: false });
    } catch (e) {
      if (e instanceof ControllerConflictError) {
        setConflict({ conflicts: e.conflicts, showDialog: true });
      } else {
        throw e;
      }
    }
  }, [onSave]);

  const dismissConflict = useCallback(() => {
    setConflict({ conflicts: [], showDialog: false });
  }, []);

  return { conflict, save, override, dismissConflict };
}
