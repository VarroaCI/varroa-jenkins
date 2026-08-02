import { withTheme } from "@rjsf/core";
import type { ThemeProps } from "@rjsf/core";
import TextField from "../form/TextField";
import NumberField from "../form/NumberField";
import CheckboxField from "../form/CheckboxField";
import SelectField from "../form/SelectField";
import ObjectGroup from "../form/ObjectGroup";
import ArrayGroup from "../form/ArrayGroup";

const theme: ThemeProps = {
  widgets: {
    TextWidget: TextField,
    BaseInput: TextField,
    NumberWidget: NumberField,
    CheckboxWidget: CheckboxField,
    SelectWidget: SelectField,
  },
  templates: {
    ObjectFieldTemplate: ObjectGroup,
    ArrayFieldTemplate: ArrayGroup,
  },
};

export const VarroaForm = withTheme(theme);
