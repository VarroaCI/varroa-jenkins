import type { FederatedCopySource } from "../lib/federatedCopy";
import styles from "./FederatedCopyNote.module.css";

interface Props {
  /** objectNoun names the object, for example "Jenkins role". */
  objectNoun: string;
  source: FederatedCopySource;
}

/** FederatedCopyNote states that an object is a federated copy and names its source. */
export default function FederatedCopyNote({ objectNoun, source }: Props) {
  return (
    <div className={styles.note} role="note">
      This {objectNoun} is a federated copy of {source.kind} {source.names.join(", ")}. Federation
      replaces each change to the copy. Change the source on the core cluster.
    </div>
  );
}
