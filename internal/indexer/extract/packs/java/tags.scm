(package_declaration [(identifier) (scoped_identifier)] @package)

; Definitions.
(class_declaration name: (identifier) @name body: (_) @body) @def.class
(record_declaration name: (identifier) @name body: (_) @body) @def.class
(interface_declaration name: (identifier) @name body: (_) @body) @def.interface
(annotation_type_declaration name: (identifier) @name body: (_) @body) @def.interface
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(enum_constant name: (identifier) @name) @def.enum_member
(constructor_declaration name: (identifier) @name) @def.constructor
(compact_constructor_declaration name: (identifier) @name) @def.constructor
(method_declaration name: (identifier) @name !body) @def.method.decl
(method_declaration name: (identifier) @name) @def.method
(field_declaration declarator: (variable_declarator name: (identifier) @name) @def.field)
(constant_declaration declarator: (variable_declarator name: (identifier) @name) @def.const)

; Imports.
(import_declaration [(identifier) (scoped_identifier)] @import.path (asterisk) @import.wildcard) @import
(import_declaration [(identifier) (scoped_identifier)] @import.path) @import

; References.
(method_invocation !object name: (identifier) @name) @ref.call
(method_invocation object: (_) @ref.qualifier name: (identifier) @name) @ref.call
(object_creation_expression type: (type_identifier) @name) @ref.instantiate
(object_creation_expression type: (generic_type (type_identifier) @name)) @ref.instantiate
(object_creation_expression type: (scoped_type_identifier) @name) @ref.instantiate
(superclass (type_identifier) @name) @ref.extends
(superclass (generic_type (type_identifier) @name)) @ref.extends
(superclass (scoped_type_identifier) @name) @ref.extends
(super_interfaces (type_list (type_identifier) @name)) @ref.implements
(super_interfaces (type_list (generic_type (type_identifier) @name))) @ref.implements
(extends_interfaces (type_list (type_identifier) @name)) @ref.extends
(extends_interfaces (type_list (generic_type (type_identifier) @name))) @ref.extends
(marker_annotation name: (identifier) @name) @ref.decorator
(annotation name: (identifier) @name) @ref.decorator
