; Classes, categories and protocols. Methods of an @implementation get
; the class as receiver through the @scope.
(class_interface
  . (identifier) @name
  [(property_declaration) (method_declaration) (instance_variables)] @body) @def.class
(class_interface . (identifier) @name) @def.class
(protocol_declaration
  . (identifier) @name
  [(property_declaration) (method_declaration)] @body) @def.protocol
(protocol_declaration . (identifier) @name) @def.protocol
(class_implementation . (identifier) @scope.name) @scope
(method_declaration (identifier) @name) @def.method.decl
(method_definition (identifier) @name (compound_statement) @body) @def.method
(property_declaration
  (struct_declaration (struct_declarator [
    (identifier) @name
    (pointer_declarator declarator: (identifier) @name)]))) @def.property

; C declarations.
(function_definition declarator: (function_declarator declarator: (identifier) @name)) @def.func
(translation_unit
  (declaration declarator: (function_declarator declarator: (identifier) @name)) @def.func.decl)
(struct_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(enum_specifier name: (type_identifier) @name body: (_) @body) @def.enum
(enumerator name: (identifier) @name) @def.enum_member
(type_definition declarator: (type_identifier) @name) @def.type_alias
(preproc_def name: (identifier) @name) @def.macro
(preproc_function_def name: (identifier) @name) @def.macro
(translation_unit (declaration declarator: [
  (identifier) @name @def.var
  (init_declarator declarator: (identifier) @name) @def.var
  (pointer_declarator declarator: (identifier) @name) @def.var]))

; #import and #include.
(preproc_include path: (_) @import.path) @include

; References: [receiver method], f(), superclass and adopted protocols.
(message_expression receiver: (_) @ref.qualifier method: (identifier) @name) @ref.call
(call_expression function: (identifier) @name) @ref.call
(class_interface superclass: (identifier) @name) @ref.extends
(class_interface (parameterized_arguments (type_name (identifier) @name))) @ref.implements
(protocol_declaration (protocol_reference_list (identifier) @name)) @ref.extends
