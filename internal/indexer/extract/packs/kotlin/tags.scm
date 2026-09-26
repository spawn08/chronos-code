(package_header (identifier) @package)

; Definitions. The grammar has few field names, so names are the direct
; identifier children.
(class_declaration "interface" (type_identifier) @name (class_body)? @body) @def.interface
(class_declaration (type_identifier) @name (enum_class_body) @body) @def.enum
(class_declaration (type_identifier) @name (class_body) @body) @def.class
(class_declaration (type_identifier) @name) @def.class
(object_declaration (type_identifier) @name (class_body)? @body) @def.class
(enum_entry (simple_identifier) @name) @def.enum_member
(type_alias (type_identifier) @name) @def.type_alias
(secondary_constructor "constructor" @name) @def.constructor
(function_declaration (simple_identifier) @name (function_body) @body) @def.func
(function_declaration (simple_identifier) @name) @def.func.decl
(class_parameter (binding_pattern_kind) (simple_identifier) @name) @def.property

; Properties: top level and in class bodies (not locals).
(property_declaration
  (modifiers (property_modifier) @_const)
  (variable_declaration (simple_identifier) @name)
  (#eq? @_const "const")) @def.const
(source_file (property_declaration (variable_declaration (simple_identifier) @name)) @def.var)
(class_body (property_declaration (variable_declaration (simple_identifier) @name)) @def.property)

; Imports.
(import_header (identifier) @import.path (wildcard_import) @import.wildcard) @import
(import_header (identifier) @import.path (import_alias (type_identifier) @import.alias)) @import
(import_header (identifier) @import.path) @import

; References.
(call_expression . (simple_identifier) @name) @ref.call
(call_expression
  . (navigation_expression
      . (_) @ref.qualifier
      (navigation_suffix (simple_identifier) @name) .)) @ref.call
(delegation_specifier (constructor_invocation (user_type (type_identifier) @name))) @ref.extends
(delegation_specifier (user_type (type_identifier) @name)) @ref.implements
(annotation (user_type (type_identifier) @name)) @ref.decorator
(annotation (constructor_invocation (user_type (type_identifier) @name))) @ref.decorator
