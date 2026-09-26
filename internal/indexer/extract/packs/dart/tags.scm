(library_name (dotted_identifier_list) @package)

; Definitions. A function's body is the signature's next sibling.
(class_definition name: (identifier) @name body: (_) @body) @def.class
(mixin_declaration (identifier) @name (class_body) @body) @def.trait
(extension_declaration name: (identifier) @name body: (_) @body) @def.class
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(enum_constant name: (identifier) @name) @def.enum_member
(type_alias (type_identifier) @name) @def.type_alias
((method_signature (function_signature name: (identifier) @name) @def.func) . (function_body) @body)
((method_signature (getter_signature name: (identifier) @name) @def.property) . (function_body) @body)
((method_signature (setter_signature name: (identifier) @name) @def.property) . (function_body) @body)
(declaration (function_signature name: (identifier) @name) @def.func.decl)
(constructor_signature name: (identifier) name: (identifier) @name) @def.constructor
(constructor_signature name: (identifier) @name) @def.constructor
(factory_constructor_signature (identifier) (identifier) @name) @def.constructor
(program (function_signature name: (identifier) @name) @def.func . (function_body) @body)
(class_body (declaration (initialized_identifier_list (initialized_identifier (identifier) @name) @def.field)))
(class_body (declaration (static_final_declaration_list (static_final_declaration (identifier) @name) @def.field)))
(program (initialized_identifier_list (initialized_identifier (identifier) @name) @def.var))
(program (static_final_declaration_list (static_final_declaration (identifier) @name) @def.var))

; Imports and exports.
(import_or_export
  (library_import (import_specification (configurable_uri (uri (string_literal) @import.path))))) @import
(import_or_export
  (library_import (import_specification
    (configurable_uri (uri (string_literal) @import.path))
    (identifier) @import.alias))) @import
(import_or_export
  (library_import (import_specification
    (configurable_uri (uri (string_literal) @import.path))
    (combinator "show" (identifier) @import.name)))) @import
(import_or_export
  (library_export (configurable_uri (uri (string_literal) @export.source))) @export.all) @export

; References: a call is a name followed by an argument selector.
((identifier) @ref.qualifier
  . (selector (unconditional_assignable_selector (identifier) @name))
  . (selector (argument_part)) @ref.call)
((selector (unconditional_assignable_selector (identifier) @name)) . (selector (argument_part)) @ref.call)
((identifier) @name . (selector (argument_part)) @ref.call)
(superclass (type_identifier) @name) @ref.extends
(mixins (type_identifier) @name) @ref.implements
(interfaces (type_identifier) @name) @ref.implements
(annotation name: (identifier) @name) @ref.decorator

; Type uses.
((type_identifier) @name @ref.type)

; Binding hints: typed parameters, variables and fields; final x = Foo().
(formal_parameter (type_identifier) @hint.type name: (identifier) @hint.name) @hint
(initialized_variable_definition (type_identifier) @hint.type name: (identifier) @hint.name) @hint
(initialized_variable_definition
  name: (identifier) @hint.name
  value: (identifier) @hint.call
  . (selector (argument_part))) @hint
(declaration
  (type_identifier) @hint.type
  (initialized_identifier_list (initialized_identifier . (identifier) @hint.name))) @hint
