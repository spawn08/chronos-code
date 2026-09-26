[(namespace_declaration name: (_) @package)
 (file_scoped_namespace_declaration name: (_) @package)]

; Definitions.
(namespace_declaration name: (_) @name body: (_) @body) @def.namespace
(file_scoped_namespace_declaration name: (_) @name) @def.namespace
(class_declaration name: (identifier) @name body: (_) @body) @def.class
(record_declaration name: (identifier) @name) @def.class
(struct_declaration name: (identifier) @name body: (_) @body) @def.struct
(interface_declaration name: (identifier) @name body: (_) @body) @def.interface
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(enum_member_declaration name: (identifier) @name) @def.enum_member
(delegate_declaration name: (identifier) @name) @def.type_alias
(constructor_declaration name: (identifier) @name) @def.constructor
(destructor_declaration name: (identifier) @name) @def.method
(method_declaration name: (identifier) @name !body) @def.method.decl
(method_declaration name: (identifier) @name) @def.method
(property_declaration name: (identifier) @name) @def.property
(event_declaration name: (identifier) @name) @def.property
(field_declaration
  (modifier) @_const
  (variable_declaration (variable_declarator name: (identifier) @name) @def.const)
  (#eq? @_const "const"))
(field_declaration (variable_declaration (variable_declarator name: (identifier) @name) @def.field))
(event_field_declaration (variable_declaration (variable_declarator name: (identifier) @name) @def.property))

; Imports: using X; using A = X; using static X;
(using_directive "static" [(identifier) (qualified_name)] @import.path @import.wildcard) @import
(using_directive name: (identifier) @import.alias [(identifier) (qualified_name)] @import.path) @import
(using_directive !name [(identifier) (qualified_name)] @import.path) @import

; References. A base named I + capital letter is taken as an interface.
(invocation_expression function: (identifier) @name) @ref.call
(invocation_expression
  function: (member_access_expression expression: (_) @ref.qualifier name: (identifier) @name)) @ref.call
(invocation_expression function: (generic_name (identifier) @name)) @ref.call
(invocation_expression
  function: (member_access_expression expression: (_) @ref.qualifier name: (generic_name (identifier) @name))) @ref.call
(object_creation_expression type: (identifier) @name) @ref.instantiate
(object_creation_expression type: (generic_name (identifier) @name)) @ref.instantiate
(object_creation_expression type: (qualified_name qualifier: (_) @ref.qualifier name: (identifier) @name)) @ref.instantiate
(base_list (identifier) @name (#match? @name "^I[A-Z]")) @ref.implements
(base_list (generic_name (identifier) @name) (#match? @name "^I[A-Z]")) @ref.implements
(base_list (identifier) @name) @ref.extends
(base_list (generic_name (identifier) @name)) @ref.extends
(base_list (qualified_name qualifier: (_) @ref.qualifier name: (identifier) @name)) @ref.extends
(attribute name: (identifier) @name) @ref.decorator
(attribute name: (qualified_name qualifier: (_) @ref.qualifier name: (identifier) @name)) @ref.decorator

; Type uses: types are plain identifiers in type positions.
(variable_declaration type: (identifier) @name @ref.type)
(parameter type: (identifier) @name @ref.type)
(property_declaration type: (identifier) @name @ref.type)
(method_declaration returns: (identifier) @name @ref.type)
(type_argument_list (identifier) @name @ref.type)
(variable_declaration type: (generic_name (identifier) @name) @ref.type)
(parameter type: (generic_name (identifier) @name) @ref.type)
(parameter type: (qualified_name qualifier: (_) @ref.qualifier name: (identifier) @name) @ref.type)

; Binding hints: typed parameters, locals, fields and properties; new Foo().
(parameter type: (_) @hint.type name: (identifier) @hint.name) @hint
(variable_declaration type: (_) @hint.type (variable_declarator name: (identifier) @hint.name)) @hint
(variable_declaration
  (variable_declarator name: (identifier) @hint.name (object_creation_expression type: (_) @hint.type))) @hint
(property_declaration type: (_) @hint.type name: (identifier) @hint.name) @hint
