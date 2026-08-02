import type { ObjectFieldTemplateProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function ObjectGroup(props: ObjectFieldTemplateProps) {
  const { title, properties } = props;
  return (
    <div className={styles.objectGroup}>
      {title && <div className={styles.objectTitle}>{title}</div>}
      {properties.map((prop) => (
        <div key={prop.content.key || prop.name}>{prop.content}</div>
      ))}
    </div>
  );
}
