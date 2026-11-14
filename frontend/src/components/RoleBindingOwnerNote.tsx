import { Link } from "react-router-dom";
import {
  roleBindingOwnerAdvice,
  roleBindingOwnerNoun,
  roleBindingOwnerRef,
  type RoleBindingOwner,
} from "../lib/roleBindingOwner";
import styles from "./RoleBindingOwnerNote.module.css";

interface Props {
  owner: RoleBindingOwner;
  /** route is the page of the owner, or null when no page shows it. */
  route: string | null;
}

/** RoleBindingOwnerNote states that the operator writes a binding and links to its owner. */
export default function RoleBindingOwnerNote({ owner, route }: Props) {
  const ref = roleBindingOwnerRef(owner);
  return (
    <div className={styles.note} role="note">
      The operator writes this role binding for {roleBindingOwnerNoun(owner)}{" "}
      {route ? <Link to={route}>{ref}</Link> : ref}.
      The operator replaces each change on its next pass. {roleBindingOwnerAdvice(owner)}
    </div>
  );
}
