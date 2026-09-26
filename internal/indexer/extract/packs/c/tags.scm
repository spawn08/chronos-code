; Definitions.
(function_definition declarator: (function_declarator declarator: (identifier) @name)) @def.func
(function_definition
  declarator: (pointer_declarator declarator: (function_declarator declarator: (identifier) @name))) @def.func
(translation_unit
  (declaration declarator: (function_declarator declarator: (identifier) @name)) @def.func.decl)
(translation_unit
  (declaration
    declarator: (pointer_declarator declarator: (function_declarator declarator: (identifier) @name))) @def.func.decl)
(struct_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(union_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(enum_specifier name: (type_identifier) @name body: (_) @body) @def.enum
(enumerator name: (identifier) @name) @def.enum_member
(field_declaration_list
  (field_declaration declarator: [
    (field_identifier) @name
    (pointer_declarator declarator: (field_identifier) @name)
    (array_declarator declarator: (field_identifier) @name)]) @def.field)
(type_definition declarator: (type_identifier) @name) @def.type_alias
(preproc_def name: (identifier) @name) @def.macro
(preproc_function_def name: (identifier) @name) @def.macro

; Top-level variables (the declarator is the definition, so int a, b;
; gives two).
(translation_unit (declaration declarator: [
  (identifier) @name @def.var
  (init_declarator declarator: (identifier) @name) @def.var
  (pointer_declarator declarator: (identifier) @name) @def.var
  (init_declarator declarator: (pointer_declarator declarator: (identifier) @name)) @def.var
  (array_declarator declarator: (identifier) @name) @def.var]))

; Includes.
(preproc_include path: (_) @import.path) @include

; References.
(call_expression function: (identifier) @name) @ref.call
(call_expression function: (field_expression argument: (_) @ref.qualifier field: (field_identifier) @name)) @ref.call
