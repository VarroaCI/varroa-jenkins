import { withTheme } from "@rjsf/core";
import type { ThemeProps } from "@rjsf/core";
import TextField from "../form/TextField";
import NumberField from "../form/NumberField";
import CheckboxField from "../form/CheckboxField";
import SelectField from "../form/SelectField";
import ObjectGroup from "../form/ObjectGroup";
import ArrayGroup from "../form/ArrayGroup";
import MapEntry from "../form/MapEntry";
import { AddButton, RemoveButton } from "../form/IconButtons";

// RJSF's stock ErrorListTemplate renders an unstyled "Errors" heading above the
// form listing every message, raw JSON Schema regex and all. Every error already
// renders on its own field, and SpecEditorCard shows a save-blocked message
// naming the offending path, so the summary is a duplicate that leaks the
// pattern source at the user. Render nothing.
function NoErrorList() {
  return null;
}

const theme: ThemeProps = {
  widgets: {
    TextWidget: TextField,
    BaseInput: TextField,
    NumberWidget: NumberField,
    CheckboxWidget: CheckboxField,
    SelectWidget: SelectField,
  },
  templates: {
    ErrorListTemplate: NoErrorList,
    ObjectFieldTemplate: ObjectGroup,
    ArrayFieldTemplate: ArrayGroup,
    WrapIfAdditionalTemplate: MapEntry,
    ButtonTemplates: {
      AddButton,
      RemoveButton,
    },
  },
};

export const VarroaForm = withTheme(theme);
