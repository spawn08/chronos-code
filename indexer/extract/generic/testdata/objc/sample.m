#import <Foundation/Foundation.h>
#import "Repo.h"

/// A user repository.
@interface UserRepo : BaseRepo <Repo, NSCopying>
@property (nonatomic, strong) NSString *name;
- (instancetype)initWithName:(NSString *)name;
+ (UserRepo *)shared;
@end

@implementation UserRepo

- (instancetype)initWithName:(NSString *)name {
    self = [super init];
    _name = name;
    [Helper check:name];
    return self;
}

+ (UserRepo *)shared {
    return [[UserRepo alloc] initWithName:@"x"];
}

@end

@protocol Repo <NSObject>
- (id)find:(NSString *)key;
@end

static int counter = 0;

void helper(int n) {
    NSLog(@"%d", n);
}
