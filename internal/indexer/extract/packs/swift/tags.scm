; Definitions. Members of an extension get the extended type as receiver.
(class_declaration declaration_kind: "class" name: (type_identifier) @name) @def.class
(class_declaration declaration_kind: "actor" name: (type_identifier) @name) @def.class
(class_declaration declaration_kind: "struct" name: (type_identifier) @name) @def.struct
(class_declaration declaration_kind: "enum" name: (type_identifier) @name) @def.enum
(class_declaration declaration_kind: "extension" (user_type (type_identifier) @scope.name)) @scope
(protocol_declaration name: (type_identifier) @name) @def.protocol
(enum_entry name: (simple_identifier) @name) @def.enum_member
(typealias_declaration name: (type_identifier) @name) @def.type_alias
(init_declaration "init" @name) @def.constructor
(deinit_declaration "deinit" @name) @def.method
(function_declaration name: (simple_identifier) @name) @def.func
(protocol_function_declaration name: (simple_identifier) @name) @def.method.decl
(class_body (property_declaration name: (pattern bound_identifier: (simple_identifier) @name)) @def.property)
(protocol_body (protocol_property_declaration name: (pattern bound_identifier: (simple_identifier) @name)) @def.property)
(source_file (property_declaration name: (pattern bound_identifier: (simple_identifier) @name)) @def.var)

; Imports.
(import_declaration (identifier) @import.path) @import

; References. Superclass and protocols are not distinguishable by syntax.
(call_expression . (simple_identifier) @name) @ref.call
(call_expression
  . (navigation_expression
      target: (_) @ref.qualifier
      suffix: (navigation_suffix suffix: (simple_identifier) @name))) @ref.call
(inheritance_specifier (user_type (type_identifier) @name)) @ref.extends
(attribute (user_type (type_identifier) @name)) @ref.decorator
