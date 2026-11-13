import { Link } from "react-router-dom";
import { controllerRoute } from "../routing";
import { rbacSpecOwnerLabel, type RbacSpecOwner } from "../lib/rbacSpecBinding";
import styles from "./RbacSpecBindingNote.module.css";

interface Props {
  /** cluster is the cluster that holds the binding and its controller. */
  cluster: string;
  owner: RbacSpecOwner;
}

/** RbacSpecBindingNote states that the operator writes a binding and links to its controller. */
export default function RbacSpecBindingNote({ cluster, owner }: Props) {
  return (
    <div className={styles.note} role="note">
      The operator writes this Jenkins role binding from the rbacSpec of controller{" "}
      <Link to={controllerRoute(cluster, owner.namespace, owner.name)}>{rbacSpecOwnerLabel(owner)}</Link>.
      The operator replaces each change on its next pass. Change the rbacSpec of the controller.
    </div>
  );
}
