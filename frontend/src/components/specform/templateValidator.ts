import Ajv2020 from "ajv/dist/2020";
import { customizeValidator } from "@rjsf/validator-ajv8";

/**
 * A pipeline-template document declares a JSON Schema 2020-12 subset
 * (design D3). The shared validator `ControllerSpecForm` uses is Ajv's
 * draft-07 build, so this dedicated instance builds Ajv on the 2020-12
 * meta-schema instead. It does not replace the shared validator.
 */
export const templateValidator = customizeValidator({ AjvClass: Ajv2020 });
